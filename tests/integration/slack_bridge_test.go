//go:build integration
// +build integration

package integration_test

// The Slack bridge end to end, against real Postgres, Dgraph and Redis, with
// Slack itself played by a local server:
//   - a signed Slack message becomes a OneCamp post led by the sender's name,
//     and a retried delivery of it does not become a second post;
//   - a thread reply becomes a comment; an edit and a deletion follow;
//   - the bridge's own messages are never taken back in;
//   - a OneCamp post goes to Slack under its author's name, and its edit and
//     deletion follow it there.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	slackBridge "github.com/akashc777/OneCamp/business/SlackBridge"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

const bridgeSecret = "integration-signing-secret"

// fakeSlack records every Web API call and answers like Slack.
type fakeSlack struct {
	mu    sync.Mutex
	calls []string // "method channel ts username text"
	next  int
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	f.next++
	ts := fmt.Sprintf("1700000000.%06d", f.next)
	f.calls = append(f.calls, strings.Join([]string{method, r.Form.Get("channel"), r.Form.Get("ts"), r.Form.Get("thread_ts"), r.Form.Get("username"), r.Form.Get("text")}, "|"))
	f.mu.Unlock()
	switch method {
	case "users.info":
		fmt.Fprint(w, `{"ok":true,"user":{"id":"U9","name":"priya","profile":{"display_name":"Priya"}}}`)
	case "chat.postMessage":
		fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (f *fakeSlack) find(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func deliver(t *testing.T, body string) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(bridgeSecret))
	mac.Write([]byte("v0:" + ts + ":" + body))
	res := slackBridge.HandleEvents(context.Background(), []byte(body), ts, "v0="+hex.EncodeToString(mac.Sum(nil)))
	if res.Status != 200 {
		t.Fatalf("delivery refused: %+v", res)
	}
}

func messageEvent(event string) string {
	return `{"type":"event_callback","team_id":"T1","event_id":"Ev` + uuid.NewString()[:8] + `","event":` + event + `}`
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSlackBridgeCarriesAConversationBothWays(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	ctx := context.Background()

	// Indexing runs in the background on every post; give it somewhere to go.
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
	restore := slackBridge.PointAtSlackForTest(slackSrv.URL + "/")
	defer restore()

	// A OneCamp channel, in both stores.
	channelUUID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`,
		channelUUID, "bridge-"+channelUUID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channelUUID.String(), "ch_name": "general"}})

	tokenEnc, _ := helpers.EncryptSecret("xoxb-test")
	secretEnc, _ := helpers.EncryptSecret(bridgeSecret)
	if err := model.SaveBridge(ctx, model.Bridge{TeamID: "T1", TeamName: "Acme", BotUserID: "U1", BotID: "B1",
		BotTokenEnc: tokenEnc, SigningSecretEnc: secretEnc}); err != nil {
		t.Fatal(err)
	}
	if err := model.CreateLink(ctx, model.Link{ID: uuid.New(), SlackChannelID: "C1", SlackChannelName: "general", ChannelUUID: channelUUID}); err != nil {
		t.Fatal(err)
	}
	slackBridge.ForgetStateForTest()
	slackBridge.Start()

	postText := func(postUUID uuid.UUID) string {
		p, err := postDomain.GetDgraphPostOnlyTextDgraph(ctx, postUUID.String())
		if err != nil || p == nil {
			return ""
		}
		return p.Text
	}
	mapped := func(ts string) *model.Message {
		m, _ := model.MessageBySlackTs(ctx, "C1", ts)
		return m
	}

	// 1. A Slack message arrives, twice (Slack retries).
	first := messageEvent(`{"type":"message","channel":"C1","user":"U9","text":"Ship *today*? <@U9>","ts":"100.1"}`)
	deliver(t, first)
	deliver(t, first)
	waitFor(t, "the Slack message to become a post", func() bool { m := mapped("100.1"); return m != nil && m.PostUUID != nil })
	root := *mapped("100.1").PostUUID
	if got := postText(root); !strings.Contains(got, "[Priya]") || !strings.Contains(got, "<strong>today</strong>") || !strings.Contains(got, "@Priya") {
		t.Fatalf("post text = %q", got)
	}
	var posts int
	_ = env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, channelUUID).Scan(&posts)
	if posts != 1 {
		t.Fatalf("a retried delivery made %d posts", posts)
	}

	// 2. A thread reply becomes a comment on that post.
	deliver(t, messageEvent(`{"type":"message","channel":"C1","user":"U9","text":"on it","ts":"100.2","thread_ts":"100.1"}`))
	waitFor(t, "the thread reply to become a comment", func() bool { m := mapped("100.2"); return m != nil && m.CommentUUID != nil })
	if m := mapped("100.2"); *m.PostUUID != root {
		t.Fatalf("reply hung on %s, want %s", m.PostUUID, root)
	}

	// 3. The bridge's own message is not taken back in.
	deliver(t, messageEvent(`{"type":"message","subtype":"bot_message","channel":"C1","bot_id":"B1","username":"Sam","text":"echo","ts":"100.3"}`))
	time.Sleep(500 * time.Millisecond)
	if mapped("100.3") != nil {
		t.Fatal("the bridge's own Slack message was bridged back")
	}

	// 4. An edit in Slack follows.
	deliver(t, messageEvent(`{"type":"message","subtype":"message_changed","channel":"C1","message":{"type":"message","user":"U9","text":"Ship tomorrow","ts":"100.1"}}`))
	waitFor(t, "the edit to arrive", func() bool { return strings.Contains(postText(root), "Ship tomorrow") })

	// 5. OneCamp → Slack: a post in the linked channel goes out under its author.
	automation, err := userBusiness.EnsureSlackBridgeBot(ctx) // any principal can author the post under test
	if err != nil {
		t.Fatal(err)
	}
	res, err := botpost.PostToChannelAsBot(ctx, channelUUID, "<p>Deploy <strong>done</strong></p>", automation)
	if err != nil {
		t.Fatal(err)
	}
	out := uuid.MustParse(res.PostUUID)
	webhookBusiness.DispatchEvent(ctx, "post.created", map[string]interface{}{
		"post_id": out.String(), "channel_id": channelUUID.String(), "author_name": "Sam Rivera", "source": "user",
	})
	waitFor(t, "chat.postMessage", func() bool { return len(slack.find("chat.postMessage")) == 1 })
	if call := slack.find("chat.postMessage")[0]; !strings.Contains(call, "|Sam Rivera|Deploy *done*") || !strings.Contains(call, "|C1|") {
		t.Fatalf("postMessage call = %q", call)
	}
	waitFor(t, "the outbound mapping", func() bool { m, _ := model.MessageByPost(ctx, out); return m != nil })

	webhookBusiness.DispatchEvent(ctx, "post.updated", map[string]interface{}{"post_id": out.String(), "channel_id": channelUUID.String()})
	waitFor(t, "chat.update", func() bool { return len(slack.find("chat.update")) == 1 })

	// Editing a Slack-born post in OneCamp does NOT go back to Slack.
	webhookBusiness.DispatchEvent(ctx, "post.updated", map[string]interface{}{"post_id": root.String(), "channel_id": channelUUID.String()})
	time.Sleep(500 * time.Millisecond)
	if n := len(slack.find("chat.update")); n != 1 {
		t.Fatalf("a Slack-born post's edit was sent back to Slack (%d updates)", n)
	}

	webhookBusiness.DispatchEvent(ctx, "post.deleted", map[string]interface{}{"post_id": out.String(), "channel_id": channelUUID.String()})
	waitFor(t, "chat.delete", func() bool { return len(slack.find("chat.delete")) == 1 })

	// 6. A deletion in Slack removes the post here.
	deliver(t, messageEvent(`{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"100.1"}`))
	waitFor(t, "the deletion to arrive", func() bool {
		var deleted bool
		_ = env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM posts WHERE id = $1`, root).Scan(&deleted)
		return deleted
	})
	if n := len(slack.find("chat.delete")); n != 1 {
		t.Fatalf("a Slack deletion echoed back to Slack (%d deletes)", n)
	}
}
