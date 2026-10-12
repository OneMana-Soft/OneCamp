//go:build integration
// +build integration

package integration_test

// Who is told about a new comment on a doc, against real Postgres and Dgraph:
// the doc's owner, its editors, whoever commented on it before, and anyone the
// comment mentions who may read the doc; never its author; never someone a
// private doc is no longer shared with; and not, for now, anyone who paused
// notifications (nobody's comment goes through a pause).
//
// It went to the commenter's own devices, and a comment by the doc's owner
// told nobody. Pushes are watched where every path sends them, so this checks
// whom each reaches without a Firebase project.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADocCommentReachesThePeopleInTheDoc -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADocCommentReachesThePeopleInTheDoc(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
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
	var pushed [][]string
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		if data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT {
			return
		}
		mu.Lock()
		pushed = append(pushed, slices.Clone(tokens))
		mu.Unlock()
	})()

	names := []string{"owner", "editor", "paused", "viewer", "commenter", "former", "reader", "stranger"}
	people := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range names {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "device-of-"+name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	// "paused" has paused notifications for the next hour.
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, notifications_paused_until)
		VALUES ($1, $2, NOW() + interval '1 hour')`, people["paused"], uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	doc := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch plan",
		"doc_private": true, "doc_public_comment": true, "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_by":       map[string]any{"uid": "_:owner"},
		"doc_editing_users":    []map[string]any{{"uid": "_:owner"}, {"uid": "_:editor"}, {"uid": "_:paused"}},
		"doc_reading_users":    []map[string]any{{"uid": "_:viewer"}, {"uid": "_:reader"}, {"uid": "_:former"}},
		"doc_commenting_users": []map[string]any{{"uid": "_:commenter"}}})
	uids := dg.Mutate(t, nodes)

	comment := func(who string, mentions ...string) {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		d, err := docBusiness.GetBasicDgraphDocByUUID(ctx, doc, uids[who])
		if err != nil {
			t.Fatal(err)
		}
		var mentioned []*dgraphStruct.DgraphUser
		for _, m := range mentions {
			mentioned = append(mentioned, &dgraphStruct.DgraphUser{Uid: uids[m], Uuid: people[m].String()})
		}
		mu.Lock()
		pushed = nil
		mu.Unlock()
		if _, err := docBusiness.CreateDocComment(ctx, d, &u, &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: doc, CommentBody: "<p>a comment</p>"}, mentioned); err != nil {
			t.Fatalf("%s commenting: %v", who, err)
		}
	}
	// reached waits for the comment's push and answers whose devices it reached.
	reached := func(t *testing.T) []string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := slices.Concat(pushed...)
			mu.Unlock()
			if len(got) > 0 {
				time.Sleep(200 * time.Millisecond) // any second batch
				mu.Lock()
				got = slices.Concat(pushed...)
				mu.Unlock()
				slices.Sort(got)
				return got
			}
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	}
	devices := func(names ...string) []string {
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, "device-of-"+n)
		}
		slices.Sort(out)
		return out
	}

	// Earlier commenters: the commenter, and "former", whom the doc is then
	// taken from.
	// Each one's push is waited for, so none lands among the next one's.
	comment("commenter")
	reached(t)
	comment("former")
	reached(t)
	if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: doc, RemoveViewers: []string{people["former"].String()}}, uids["owner"]); err != nil {
		t.Fatal(err)
	}

	t.Run("its owner's comment reaches the doc's people", func(t *testing.T) {
		comment("owner", "reader", "stranger")
		got := reached(t)
		if want := devices("editor", "commenter", "reader"); !slices.Equal(got, want) {
			t.Errorf("the owner's comment reached %v, want %v (not the owner, not someone paused, not a viewer who took no part, not someone the doc was taken from, not a stranger)", got, want)
		}
	})

	t.Run("an editor's comment reaches the others, not the editor", func(t *testing.T) {
		comment("editor")
		got := reached(t)
		if want := devices("owner", "commenter"); !slices.Equal(got, want) {
			t.Errorf("the editor's comment reached %v, want %v", got, want)
		}
	})
}
