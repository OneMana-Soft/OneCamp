//go:build integration
// +build integration

package integration_test

// A forward into a channel, and a thread reply "also sent to the channel",
// are posts in that channel, so they take the rules a post does
// (business/Send): not into an archived channel, and into an announcement
// channel only from one of its admins. Both used to check membership only.
//
// Run: go test -tags=integration ./tests/integration/ -run TestForwardAndAlsoSend -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestForwardAndAlsoSendFollowThePostRules(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Posts are indexed in the background; give them somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	sam := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, sam, sam.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	general, news, old := uuid.New(), uuid.New(), uuid.New()
	for name, ch := range map[string]uuid.UUID{"general": general, "news": news, "old": old} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, ch, name); err != nil {
			t.Fatal(err)
		}
	}
	source, announcement := uuid.New(), uuid.New()
	crew := "grp-" + uuid.NewString()
	for post, ch := range map[uuid.UUID]uuid.UUID{source: general, announcement: news} {
		if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, ch, sam); err != nil {
			t.Fatal(err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	member := []map[string]any{{"uid": "_:sam"}}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": member,
			"ch_posts": []map[string]any{{"uid": "_:source", "dgraph.type": "Post", "post_uuid": source.String(),
				"post_text": "<p>ship it</p>", "post_by": map[string]any{"uid": "_:sam"}, "post_channel": map[string]any{"uid": "_:general"},
				"post_created_at": "2026-10-01T10:00:00Z", "post_deleted_at": live}}},
		{"uid": "_:news", "dgraph.type": "Channel", "ch_uuid": news.String(), "ch_name": "news",
			"ch_private": false, "ch_deleted_at": live, "ch_post_policy": "admins_only", "ch_members": member,
			"ch_posts": []map[string]any{{"uid": "_:announcement", "dgraph.type": "Post", "post_uuid": announcement.String(),
				"post_text": "<p>we moved</p>", "post_by": map[string]any{"uid": "_:sam"}, "post_channel": map[string]any{"uid": "_:news"},
				"post_created_at": "2026-10-01T10:00:00Z", "post_deleted_at": live}}},
		{"uid": "_:old", "dgraph.type": "Channel", "ch_uuid": old.String(), "ch_name": "old",
			"ch_private": false, "ch_deleted_at": "2026-01-01T00:00:00Z", "ch_members": member},
		{"uid": "_:alex", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "alex"},
		{"uid": "_:crew", "dgraph.type": "Dm", "dm_grouping_id": crew,
			"dm_participants": []map[string]any{{"uid": "_:sam"}, {"uid": "_:alex"}}},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})

	call := func(handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asSam))
		return rec.Code, rec.Body.String()
	}
	posts := func(ch uuid.UUID) (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, ch).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	replies := func() (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM comments WHERE created_by = $1`, sam).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	forward := func(to string) (int, string) {
		return call(userController.FwdUserMessage, userAdapter.UserFwdMsgInput{
			HtmlText: "<p>ship it</p>", ChannelUuid: general.String(), PostUuid: source.String(),
			FwdTo: []userAdapter.UserAndChannelFwdMessage{{ChannelDgraphUid: to}},
		})
	}

	// Forwarding: a member of an announcement channel who isn't one of its
	// admins can't post there by forwarding, and nobody forwards into an
	// archived channel. Refused before anything is written.
	if code, body := forward(uids["news"]); code != http.StatusForbidden || !strings.Contains(body, "announcement") || posts(news) != 1 {
		t.Errorf("a forward into an announcement channel: %d %s, %d posts there", code, body, posts(news))
	}
	if code, body := forward(uids["old"]); code != http.StatusBadRequest || posts(old) != 0 {
		t.Errorf("a forward into an archived channel: %d %s, %d posts there", code, body, posts(old))
	}
	// Their own channel passes the rules, and the forward is written, in
	// Postgres and in the graph. (The graph refused every forward into a
	// channel or a group chat until its upsert's query named the variables
	// its mutation uses.)
	if code, body := forward(uids["general"]); code != http.StatusOK || posts(general) != 2 {
		t.Errorf("a forward into their own channel: %d %s, %d posts there", code, body, posts(general))
	}
	if n := graphCount(t, `{ q(func: eq(ch_uuid, "`+general.String()+`")) { n: count(ch_posts) } }`); n != 2 {
		t.Errorf("the channel holds %d posts in the graph, want 2", n)
	}
	// And into a group chat they're in.
	code, body := call(userController.FwdUserMessage, userAdapter.UserFwdMsgInput{
		HtmlText: "<p>ship it</p>", ChannelUuid: general.String(), PostUuid: source.String(),
		FwdTo: []userAdapter.UserAndChannelFwdMessage{{GrpId: crew}},
	})
	if code != http.StatusOK {
		t.Errorf("a forward into their group chat: %d %s", code, body)
	}
	if n := graphCount(t, `{ q(func: eq(dm_grouping_id, "`+crew+`")) { n: count(dm_chats) } }`); n != 1 {
		t.Errorf("the group chat holds %d messages in the graph, want 1", n)
	}
	// One list of nodes is written, not an object holding them, which made a
	// node of its own on every forward.
	if n := graphCount(t, `{ q(func: has(chats)) { n: count(uid) } }`); n != 0 {
		t.Errorf("%d stray nodes holding the forwarded messages", n)
	}

	reply := func(post uuid.UUID, alsoSend bool) (int, string) {
		return call(postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{
			PostUuid: post.String(), HTMLText: "<p>noted</p>", AlsoSendToChannel: alsoSend,
		})
	}

	// "Also send to channel" in an announcement channel they don't run is
	// refused before anything is written; the reply alone is theirs to make.
	if code, body := reply(announcement, true); code != http.StatusForbidden || posts(news) != 1 || replies() != 0 {
		t.Errorf("also sent to an announcement channel: %d %s, %d posts there, %d replies", code, body, posts(news), replies())
	}
	if code, body := reply(announcement, false); code != http.StatusOK || replies() != 1 {
		t.Errorf("a reply in an announcement channel's thread: %d %s, %d replies", code, body, replies())
	}
	// In their own channel, the reply and the post both.
	if code, body := reply(source, true); code != http.StatusOK || replies() != 2 || posts(general) != 3 {
		t.Errorf("also sent to their own channel: %d %s, %d replies, %d posts there", code, body, replies(), posts(general))
	}
}

// graphCount runs a query whose block q returns one count named n.
func graphCount(t *testing.T, query string) int {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	var out struct {
		Q []struct {
			N int `json:"n"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Q) == 0 {
		return 0
	}
	return out.Q[0].N
}
