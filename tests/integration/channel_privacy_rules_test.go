//go:build integration
// +build integration

package integration_test

// Who may make a private channel public, against real Postgres, Redis and
// Dgraph. Any of its moderators could, with one switch, and its whole history
// and files were then readable by everyone in the workspace, with nobody told.
// Now only a workspace admin opens a private channel; any moderator may still
// close one; either way a line in the channel says who did and what it means.
// And a channel made private is private at once to everyone outside it: the
// gates read a cache of the channel, which kept saying "public" until it
// expired.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOnlyAnAdminOpensAPrivateChannel -v

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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyAnAdminOpensAPrivateChannel(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	// The bot that posts the privacy lines is kept for the process: resolve it
	// in this test's databases, and leave the next test to resolve its own.
	userBusiness.ForgetAutomationBotForTest()
	t.Cleanup(userBusiness.ForgetAutomationBotForTest)
	if _, err := userBusiness.EnsureAutomationBot(ctx); err != nil {
		t.Fatalf("the automation bot: %v", err)
	}

	// #leadership is private. Mod moderates it; Boss is a workspace admin who
	// moderates it too; Outsider is in the workspace, not in the channel.
	people := map[string]uuid.UUID{}
	channel := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'leadership', true)`, channel); err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	for _, name := range []string{"mod", "boss", "outsider"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('boss@acme.test')`); err != nil {
		t.Fatal(err)
	}
	zero := "0001-01-01T00:00:00Z"
	dg.Mutate(t, append(nodes, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(),
		"ch_name": "leadership", "ch_private": true, "ch_deleted_at": zero,
		"ch_members":    []map[string]any{{"uid": "_:mod"}, {"uid": "_:boss"}},
		"ch_moderators": []map[string]any{{"uid": "_:mod"}, {"uid": "_:boss"}},
		"ch_posts": []map[string]any{{"dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": "<p>Reorg on Friday</p>",
			"post_deleted_at": zero, "post_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
			"post_by": map[string]any{"uid": "_:mod"}, "post_channel": map[string]any{"uid": "_:ch"}}}}))

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
	setPrivate := func(name string, private bool) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(channelAdapter.UpdateChannelInfo{ChannelUuid: channel.String(), ChannelName: "leadership", ChannelPrivate: private})
		rec := httptest.NewRecorder()
		channelController.UpdateChannelInfo(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		return rec.Code, rec.Body.String()
	}
	private := func() (pg bool, graph string) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT ch_private FROM channels WHERE id = $1`, channel).Scan(&pg); err != nil {
			t.Fatal(err)
		}
		return pg, string(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_private } }`))
	}
	// lines is every message in the channel, one after another.
	lines := func() string {
		t.Helper()
		var out struct {
			Q []struct {
				Posts []struct {
					Text string `json:"post_text"`
				} `json:"ch_posts"`
			} `json:"q"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_posts { post_text } } }`), &out)
		var texts []string
		for _, q := range out.Q {
			for _, p := range q.Posts {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	outsiderReads := func() (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("channel_uuid", channel.String())
		rec := httptest.NewRecorder()
		postController.GetLatestPosts(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(as("outsider"), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}

	t.Run("a moderator can't make a private channel public", func(t *testing.T) {
		if code, body := setPrivate("mod", false); code != http.StatusForbidden {
			t.Errorf("the moderator's switch: %d %s, want 403", code, body)
		}
		if pg, graph := private(); !pg || !strings.Contains(graph, `"ch_private":true`) {
			t.Errorf("the channel is no longer private: %v %s", pg, graph)
		}
		if strings.Contains(lines(), "made this channel") {
			t.Errorf("a line for a change that didn't happen: %s", lines())
		}
	})

	t.Run("a workspace admin can, and the channel is told", func(t *testing.T) {
		if code, body := setPrivate("boss", false); code != http.StatusOK {
			t.Fatalf("the admin's switch: %d %s", code, body)
		}
		if pg, graph := private(); pg || !strings.Contains(graph, `"ch_private":false`) {
			t.Errorf("the channel is still private: %v %s", pg, graph)
		}
		if got := lines(); !strings.Contains(got, "<strong>boss</strong> made this channel public") {
			t.Errorf("no line saying so: %s", got)
		}
		if code, body := outsiderReads(); code != http.StatusOK || !strings.Contains(body, "Reorg on Friday") {
			t.Fatalf("someone outside it reads a public channel: %d %s", code, body)
		}
	})

	t.Run("a moderator can close it again, and it is closed at once", func(t *testing.T) {
		if code, body := setPrivate("mod", true); code != http.StatusOK {
			t.Fatalf("the moderator closing it: %d %s", code, body)
		}
		if got := lines(); !strings.Contains(got, "<strong>mod</strong> made this channel private") {
			t.Errorf("no line saying so: %s", got)
		}
		if code, body := outsiderReads(); code == http.StatusOK || strings.Contains(body, "Reorg on Friday") {
			t.Errorf("someone outside it still reads it: %d %s", code, body)
		}
	})
}
