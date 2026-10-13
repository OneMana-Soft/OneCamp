//go:build integration
// +build integration

package integration_test

// A thread reply sent to its channel too ("Also send to #channel"), against
// real Postgres, Redis and Dgraph: two messages, the reply and its copy in the
// channel, which used to notify twice (the people it mentions heard of each)
// and went separate ways after (an edit or a delete reached the reply only,
// so a deleted reply stayed readable as its copy). Now the copy names nobody
// of its own, and follows the reply's edits and deletion.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAReplyAlsoSentToTheChannelIsOneMessage -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAReplyAlsoSentToTheChannelIsOneMessage(t *testing.T) {
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

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// #launch: Bo asked; Ada answers in the thread mentioning Cy, and sends it
	// to the channel too. Cy hears of mentions only.
	people := map[string]uuid.UUID{}
	uids := map[string]string{}
	channel, post := uuid.New(), uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel)
	for name, level := range map[string]string{"ada": "all", "bo": "all", "cy": "mention"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`, id, channel, level)
	}
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, channel, people["bo"])
	zero := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for name, id := range people {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	got := dg.Mutate(t, append(nodes, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(),
		"ch_name": "launch", "ch_private": false, "ch_deleted_at": zero,
		"ch_members": []map[string]any{{"uid": "_:ada"}, {"uid": "_:bo"}, {"uid": "_:cy"}},
		"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": post.String(),
			"post_text": "<p>Launch on Friday?</p>", "post_deleted_at": zero, "post_created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
			"post_by": map[string]any{"uid": "_:bo"}, "post_channel": map[string]any{"uid": "_:ch"}}}}))
	for name := range people {
		uids[name] = got[name]
	}
	asAda := func() context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["ada"])
		if err != nil || pg == nil {
			t.Fatalf("load ada: %v", err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["ada"].String())
		if err != nil || gr == nil {
			t.Fatalf("load ada's node: %v", err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	send := func(handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asAda()))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Data
	}
	// theCopy is the copy in the channel: its text, deletion time in the
	// graph, and who it names.
	type copyState struct {
		Uuid    string `json:"post_uuid"`
		Text    string `json:"post_text"`
		Deleted string `json:"post_deleted_at"`
		Named   []struct {
			Users []struct {
				Uuid string `json:"user_uuid"`
			} `json:"mention_users"`
		} `json:"post_mentions"`
	}
	theCopy := func() copyState {
		t.Helper()
		var out struct {
			Q []struct {
				Posts []copyState `json:"ch_posts"`
			} `json:"q"`
		}
		raw := dg.Query(t, `{ q(func: eq(ch_uuid, "`+channel.String()+`")) { ch_posts { post_uuid post_text post_deleted_at post_mentions { mention_users { user_uuid } } } } }`)
		_ = json.Unmarshal(raw, &out)
		for _, q := range out.Q {
			for _, p := range q.Posts {
				if p.Uuid != post.String() {
					return p
				}
			}
		}
		t.Fatalf("no copy in the channel: %s", raw)
		return copyState{}
	}

	html := fmt.Sprintf(`<p><span data-type="mention" data-id="%s@%s">@cy</span> Friday works</p>`, people["cy"], uids["cy"])
	reply := send(postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{
		PostUuid: post.String(), HTMLText: html, AlsoSendToChannel: true})["comment_id"].(string)

	t.Run("the people the reply mentions hear of it once", func(t *testing.T) {
		waitFor(t, "the copy's push", func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL {
					return true
				}
			}
			return false
		})
		time.Sleep(500 * time.Millisecond) // the reply's would be in by now
		mu.Lock()
		told := map[string][]string{}
		for _, p := range pushes {
			kind := p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE]
			told[kind] = append(told[kind], p.tokens...)
		}
		mu.Unlock()
		if !slices.Contains(told[firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT], "tok-cy") {
			t.Errorf("the reply didn't reach cy: %v", told)
		}
		if slices.Contains(told[firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL], "tok-cy") {
			t.Errorf("the copy told cy of the mention a second time: %v", told)
		}
		if c := theCopy(); len(c.Named) != 0 && len(c.Named[0].Users) != 0 {
			t.Errorf("the copy names %v of its own", c.Named)
		}
	})

	t.Run("an edit to the reply reaches the copy", func(t *testing.T) {
		send(postController.UpdateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply, PostUuid: post.String(), HTMLText: "<p>Friday works, 10am</p>"})
		if c := theCopy(); c.Text != "<p>Friday works, 10am</p>" {
			t.Errorf("the copy reads %q", c.Text)
		}
	})

	t.Run("deleting the reply deletes the copy", func(t *testing.T) {
		send(postController.DeleteCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply, PostUuid: post.String()})
		c := theCopy()
		if !strings.HasPrefix(c.Deleted, "20") {
			t.Errorf("the copy is still live in the graph: %+v", c)
		}
		var live int
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1 AND deleted_at IS NULL AND id <> $2`, channel, post).Scan(&live); err != nil || live != 0 {
			t.Errorf("%d copies live in Postgres (%v)", live, err)
		}
	})
}
