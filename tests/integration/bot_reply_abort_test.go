//go:build integration
// +build integration

package integration_test

// An AI reply dropped while it was being written is gone on reload, against
// real Postgres and Dgraph (V-578).
//
// A streamed reply (business/BotPost: a comment on a post, or a post in a
// channel) is written with its first words and grows as they come. When the
// run is dropped (the asker stops it, or the model fails), Abort took it back
// with the rollback hard delete: the Postgres row went, the graph node stayed
// with no deleted time. Every read is the graph's, so the reply came back on
// reload, counted on its thread, and the owner's rule (every delete is soft)
// was broken by the one delete a person never asked for.
//
// Now Abort goes through the shared soft delete of its record, as a person's
// delete does: the row keeps a deleted time, the node takes one, and the
// thread's replies, its count and the channel's posts never show it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAbortedReplyIsGoneOnReload -v

import (
	"context"
	"database/sql"
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

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnAbortedReplyIsGoneOnReload(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.RecordMqtt(t)
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

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// bo asked the AI coworker in #launch, on his post and in the channel.
	bo := uuid.New()
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'bo@acme.test', 'bo', 'bo', NOW(), NOW())`, bo)
	channel := uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel)
	asked := uuid.New()
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, asked, channel, bo)
	bot, err := userBusiness.EnsureAgentBot(ctx, uuid.New(), "OneCamp AI", "")
	if err != nil || bot == nil {
		t.Fatalf("the coworker's principal: %v", err)
	}
	zero := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo.String(), "user_name": "bo"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:bo"}, {"uid": bot.DgraphUID}},
			"ch_posts": []map[string]any{{"uid": "_:asked", "dgraph.type": "Post", "post_uuid": asked.String(),
				"post_text": "<p>@OneCamp AI what are the numbers?</p>", "post_deleted_at": zero,
				"post_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
				"post_by":         map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"}}}},
	})

	as := func() context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, bo)
		if err != nil || pg == nil {
			t.Fatalf("load bo: %v", err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, bo.String())
		if err != nil || gr == nil {
			t.Fatalf("load bo's node: %v", err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	// reload answers a GET as the web app's reload makes it, as bo.
	reload := func(handler http.HandlerFunc, param, value string) string {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add(param, value)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(as(), chi.RouteCtxKey, rc)))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s=%s: %d %s", param, value, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	// rowKept says what is wrong with a soft-deleted row, or "".
	rowKept := func(table string, id uuid.UUID) string {
		t.Helper()
		var at sql.NullTime
		err := env.PG.QueryRow(`SELECT deleted_at FROM `+table+` WHERE id = $1`, id).Scan(&at)
		switch {
		case err == sql.ErrNoRows:
			return "the " + table + " row is gone for good; a delete is soft (deleted_at set)"
		case err != nil:
			t.Fatal(err)
		case !at.Valid:
			return "the " + table + " row has no deleted time"
		}
		return ""
	}
	// nodeDeleted reads the graph node's deleted time by its uuid predicate.
	nodeDeleted := func(uuidPred, deletedPred, id string) (found, deleted bool) {
		t.Helper()
		raw := dg.Query(t, `{ n(func: eq(`+uuidPred+`, "`+id+`")) { at: `+deletedPred+` } }`)
		var res struct {
			N []struct {
				At time.Time `json:"at"`
			} `json:"n"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if len(res.N) == 0 {
			return false, false
		}
		return true, res.N[0].At.After(time.Unix(0, 0))
	}
	// newest is the id of the bot's newest row in table, written just now.
	newest := func(table string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(`SELECT id FROM `+table+` WHERE created_by = $1 ORDER BY created_at DESC LIMIT 1`,
			bot.UserID).Scan(&id); err != nil {
			t.Fatalf("the streamed %s row: %v", table, err)
		}
		return id
	}
	// commentCount is the thread's count as the channel's posts carry it.
	commentCount := func(body, post string) uint64 {
		t.Helper()
		var doc any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatal(err)
		}
		var n uint64
		var walk func(v any) bool
		walk = func(v any) bool {
			switch v := v.(type) {
			case map[string]any:
				if v["post_uuid"] == post {
					if c, ok := v["post_comment_count"].(float64); ok {
						n = uint64(c)
					}
					return true
				}
				for _, x := range v {
					if walk(x) {
						return true
					}
				}
			case []any:
				for _, x := range v {
					if walk(x) {
						return true
					}
				}
			}
			return false
		}
		if !walk(doc) {
			t.Fatalf("the channel's posts don't hold %s: %s", post, body)
		}
		return n
	}

	t.Run("a reply on a post", func(t *testing.T) {
		stream, err := botpost.BeginPostCommentStream(ctx, asked, bot, "")
		if err != nil {
			t.Fatal(err)
		}
		stream.Push(ctx, "The numbers")
		reply := newest("comments")
		if found, _ := nodeDeleted("comment_uuid", "comment_deleted_at", reply.String()); !found {
			t.Fatalf("the streamed reply %s has no node; the stream didn't write it", reply)
		}
		if got := commentCount(reload(postController.GetLatestPosts, "channel_uuid", channel.String()), asked.String()); got != 1 {
			t.Fatalf("the thread counts %d replies while the reply is being written; want 1", got)
		}
		stream.Abort(ctx)

		if why := rowKept("comments", reply); why != "" {
			t.Error("the dropped reply: " + why)
		}
		if found, deleted := nodeDeleted("comment_uuid", "comment_deleted_at", reply.String()); found && !deleted {
			t.Error("the dropped reply's node has no deleted time: every read is the graph's, so it comes back on reload")
		}
		if body := reload(postController.PostByUUIDWithAllComments, "post_uuid", asked.String()); strings.Contains(body, reply.String()) {
			t.Errorf("the thread's replies show the dropped reply on reload: %s", body)
		}
		if got := commentCount(reload(postController.GetLatestPosts, "channel_uuid", channel.String()), asked.String()); got != 0 {
			t.Errorf("the thread counts %d replies on reload; want 0, the dropped reply isn't one", got)
		}
		at, err := threadIndex.LatestReply(ctx, asked.String())
		if err != nil {
			t.Fatal(err)
		}
		if !at.IsZero() {
			t.Errorf("the Threads index has a reply at %v; want none, the reply was dropped", at)
		}
	})

	t.Run("a post in the channel", func(t *testing.T) {
		stream, err := botpost.BeginChannelStream(ctx, channel, bot, "")
		if err != nil {
			t.Fatal(err)
		}
		stream.Push(ctx, "Here is the plan")
		post := newest("posts")
		if body := reload(postController.GetLatestPosts, "channel_uuid", channel.String()); !strings.Contains(body, post.String()) {
			t.Fatalf("the channel doesn't show the post while it is being written: %s", body)
		}
		stream.Abort(ctx)

		if why := rowKept("posts", post); why != "" {
			t.Error("the dropped post: " + why)
		}
		if found, deleted := nodeDeleted("post_uuid", "post_deleted_at", post.String()); found && !deleted {
			t.Error("the dropped post's node has no deleted time: every read is the graph's, so it comes back on reload")
		}
		if body := reload(postController.GetLatestPosts, "channel_uuid", channel.String()); strings.Contains(body, post.String()) {
			t.Errorf("the channel's posts show the dropped post on reload: %s", body)
		}
	})
}
