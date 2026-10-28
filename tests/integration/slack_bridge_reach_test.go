//go:build integration
// +build integration

package integration_test

// The Slack bridge carries only what whoever linked it may read, against real
// Postgres, Dgraph and Redis, with Slack played by a local server
// (slack_bridge_test.go's fakeSlack):
//   - an admin who can't read a private channel can't link it: they are told
//     so in words that don't name it, and nothing is asked of Slack;
//   - an admin in a private channel links it, and its posts go to Slack;
//   - the admin page names that channel only to an admin who can read it
//     (anyone else sees "private channel (linked by <name>)"), and the audit
//     log's line for the link names it to nobody;
//   - once whoever linked it can't read the channel (taken out of it), its
//     posts stop going to Slack, but a post deleted in OneCamp still comes
//     down from Slack (a deletion carries nothing out);
//   - a link made before this by an admin who couldn't read the channel
//     carries nothing of it.
//
// What the bridge sends out was held to nothing: an admin outside a private
// channel could link it and read it in Slack, which webhooks (reach.go) and
// workflows (access.go) already refuse.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSlackBridgeCarriesOnlyWhatItsLinkerCanRead -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	slackBridge "github.com/akashc777/OneCamp/business/SlackBridge"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	slackBridgeController "github.com/akashc777/OneCamp/controllers/SlackBridge"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSlackBridgeCarriesOnlyWhatItsLinkerCanRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	slack := &fakeSlack{}
	slackSrv := httptest.NewServer(slack)
	defer slackSrv.Close()
	defer slackBridge.PointAtSlackForTest(slackSrv.URL + "/")()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Ada and Max are admins. Ada is in the private channels #exec, #board and
	// #ops; Max is in none of them.
	people := map[string]uuid.UUID{"ada": uuid.New(), "max": uuid.New()}
	channels := map[string]uuid.UUID{"exec": uuid.New(), "board": uuid.New(), "ops": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	for name, id := range channels {
		exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, true)`, id, name+"-"+id.String()[:8])
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "Channel", "ch_uuid": id.String(), "ch_name": name,
			"ch_private": true, "ch_members": []map[string]any{{"uid": "_:ada"}}})
	}
	uids := dg.Mutate(t, nodes)

	tokenEnc, _ := helpers.EncryptSecret("xoxb-test")
	secretEnc, _ := helpers.EncryptSecret(bridgeSecret)
	if err := model.SaveBridge(ctx, model.Bridge{TeamID: "T1", TeamName: "Acme", BotUserID: "U1", BotID: "B1",
		BotTokenEnc: tokenEnc, SigningSecretEnc: secretEnc}); err != nil {
		t.Fatal(err)
	}
	slackBridge.ForgetStateForTest()
	slackBridge.Start()

	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	// link has name link a Slack channel to a OneCamp channel through the
	// admin route, and answers its status and body.
	link := func(name, slackChannel, channel string) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"slack_channel_id": slackChannel, "channel_uuid": channels[channel].String()})
		rec := httptest.NewRecorder()
		slackBridgeController.CreateLink(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		return rec.Code, rec.Body.String()
	}
	linked := func(channel string) bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM slack_bridge_links WHERE channel_uuid = $1`, channels[channel]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	automation, err := userBusiness.EnsureSlackBridgeBot(ctx) // any principal can author the posts under test
	if err != nil {
		t.Fatal(err)
	}
	// post writes in channel and tells the workspace, as a post does, and
	// answers the post's id.
	post := func(channel, html string) string {
		t.Helper()
		res, err := botpost.PostToChannelAsBot(ctx, channels[channel], html, automation)
		if err != nil {
			t.Fatal(err)
		}
		webhookBusiness.DispatchEvent(ctx, "post.created", map[string]interface{}{
			"post_id": res.PostUUID, "channel_id": channels[channel].String(), "author_name": "Ada", "source": "user",
		})
		return res.PostUUID
	}
	// shown is the admin page as name sees it: its whole body, and the name it
	// shows for the link of channel.
	shown := func(name, channel string) (string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		slackBridgeController.GetStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(as(name)))
		var out struct {
			Data struct {
				Links []struct {
					ChannelUUID string `json:"channel_uuid"`
					ChannelName string `json:"channel_name"`
				} `json:"links"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("the admin page for %s: %d %s", name, rec.Code, rec.Body.String())
		}
		for _, l := range out.Data.Links {
			if l.ChannelUUID == channels[channel].String() {
				return rec.Body.String(), l.ChannelName
			}
		}
		t.Fatalf("the admin page for %s lists no link for #%s: %s", name, channel, rec.Body.String())
		return "", ""
	}
	sentTo := func(slackChannel string) int { return len(slack.find("chat.postMessage|" + slackChannel + "|")) }
	// quiet waits as long as a post takes to reach Slack when it does: event
	// listeners run on goroutines of their own.
	quiet := func() { time.Sleep(1500 * time.Millisecond) }

	t.Run("an admin who can't read a private channel can't link it, and isn't told its name", func(t *testing.T) {
		code, body := link("max", "C2", "exec")
		if code != http.StatusForbidden || !strings.Contains(body, "a channel you can read") {
			t.Errorf("max linking #exec: %d %s, want 403 and the refusal in words", code, body)
		}
		if strings.Contains(body, "exec") {
			t.Errorf("the answer names the channel max can't read: %s", body)
		}
		if linked("exec") {
			t.Error("#exec was linked by max, who can't read it")
		}
		if n := len(slack.find("conversations.")); n != 0 {
			t.Errorf("a refused link asked Slack %d things (it joins the Slack channel)", n)
		}
	})

	var boardPack string // the #board post that went to Slack
	t.Run("an admin in a private channel links it, and its posts go to Slack", func(t *testing.T) {
		if code, body := link("ada", "C3", "board"); code != http.StatusOK {
			t.Fatalf("ada linking #board: %d %s", code, body)
		}
		boardPack = post("board", "<p>Board pack is out</p>")
		waitFor(t, "ada's channel's post in Slack", func() bool { return sentTo("C3") == 1 })
	})

	t.Run("the admin page names a private channel only to an admin who can read it, and the audit log to nobody", func(t *testing.T) {
		if _, name := shown("ada", "board"); name != "board" {
			t.Errorf("the admin page shows ada, who is in #board, its link as %q, want board", name)
		}
		body, name := shown("max", "board")
		if strings.Contains(body, "board") {
			t.Errorf("the admin page names #board to max, who can't read it: %s", body)
		}
		if name != "private channel (linked by ada)" {
			t.Errorf("the admin page shows max #board's link as %q, want \"private channel (linked by ada)\"", name)
		}
		var line string
		waitFor(t, "the audit line for linking #board", func() bool {
			return env.PG.QueryRow(`SELECT summary FROM admin_audit_log WHERE action = 'slack_bridge.linked' ORDER BY created_at DESC LIMIT 1`).Scan(&line) == nil
		})
		if strings.Contains(line, "board") || !strings.Contains(line, "a private channel") {
			t.Errorf("the audit line for linking #board says %q, which every admin reads: want it to say a private channel, not its name", line)
		}
	})

	t.Run("once whoever linked it can't read the channel, its posts stop going to Slack", func(t *testing.T) {
		if err := channelBusiness.DeleteChannelMemberEdge(ctx, uids["board"], uids["ada"], people["ada"].String(), channels["board"].String()); err != nil {
			t.Fatalf("taking ada out of #board: %v", err)
		}
		post("board", "<p>Minutes after ada left</p>")
		quiet()
		if n := sentTo("C3"); n != 1 {
			t.Errorf("#board's posts went to Slack %d times, want only the one before ada, who linked it, was taken out", n)
		}
	})

	t.Run("a post deleted after that still comes down from Slack", func(t *testing.T) {
		if boardPack == "" {
			t.Skip("no #board post went to Slack")
		}
		webhookBusiness.DispatchEvent(ctx, "post.deleted", map[string]interface{}{
			"post_id": boardPack, "channel_id": channels["board"].String(),
		})
		waitFor(t, "#board's deleted post to come down from Slack", func() bool { return len(slack.find("chat.delete|C3|")) == 1 })
	})

	t.Run("a link made before this by an admin who can't read the channel carries nothing", func(t *testing.T) {
		exec(`INSERT INTO slack_bridge_links (id, slack_channel_id, slack_channel_name, channel_uuid, created_by) VALUES ($1, 'C4', 'ops-mirror', $2, $3) ON CONFLICT DO NOTHING`,
			uuid.New(), channels["ops"], people["max"])
		slackBridge.ForgetStateForTest()
		post("ops", "<p>Incident review at 4</p>")
		quiet()
		if n := sentTo("C4"); n != 0 {
			t.Errorf("#ops, linked by max who can't read it, went to Slack %d times", n)
		}
	})
}
