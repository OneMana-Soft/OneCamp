//go:build integration
// +build integration

package integration_test

// Commenting on a doc, as a member, takes being able to read it: a doc open
// to everyone's comments is open to everyone who can see it, not to anyone
// who has its id, and a deleted doc takes no comments.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocCommentRules -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocCommentRules(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// A comment is indexed in the background; give it somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]uuid.UUID{"owner": uuid.New(), "reader": uuid.New(), "commenter": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	live := "0001-01-01T00:00:00Z"
	docs := map[string]string{"private": uuid.NewString(), "closed": uuid.NewString(), "deleted": uuid.NewString()}
	nodes = append(nodes,
		// Private, but open to comments from everyone who can read it.
		map[string]any{"uid": "_:private", "dgraph.type": "Doc", "doc_uuid": docs["private"], "doc_title": "plans",
			"doc_private": true, "doc_public_comment": true, "doc_deleted_at": live,
			"doc_created_by":       map[string]any{"uid": "_:owner"},
			"doc_editing_users":    []map[string]any{{"uid": "_:owner"}},
			"doc_reading_users":    []map[string]any{{"uid": "_:reader"}},
			"doc_commenting_users": []map[string]any{{"uid": "_:commenter"}}},
		// Public, comments from the people it's shared with only.
		map[string]any{"uid": "_:closed", "dgraph.type": "Doc", "doc_uuid": docs["closed"], "doc_title": "handbook",
			"doc_private": false, "doc_public_comment": false, "doc_deleted_at": live,
			"doc_created_by":    map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}}},
		// Deleted, though it was open to everyone's comments.
		map[string]any{"uid": "_:deleted", "dgraph.type": "Doc", "doc_uuid": docs["deleted"], "doc_title": "old",
			"doc_private": false, "doc_public_comment": true, "doc_deleted_at": "2026-01-01T00:00:00Z",
			"doc_created_by":    map[string]any{"uid": "_:owner"},
			"doc_editing_users": []map[string]any{{"uid": "_:owner"}}},
	)
	uids := dg.Mutate(t, nodes)

	comment := func(who, doc string) int {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		raw, _ := json.Marshal(docAdapter.CreateOrUpdateDocCommentInput{DocUuid: docs[doc], CommentBody: "<p>noted</p>"})
		rec := httptest.NewRecorder()
		docController.CreateDocComment(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u)))
		return rec.Code
	}
	comments := func(who string) (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM comments WHERE created_by = $1`, people[who]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, c := range []struct {
		who, doc string
		want     int
	}{
		{"stranger", "private", http.StatusForbidden},
		{"reader", "private", http.StatusOK},
		{"commenter", "private", http.StatusOK},
		{"owner", "private", http.StatusOK},
		{"stranger", "closed", http.StatusForbidden},
		{"owner", "closed", http.StatusOK},
		{"stranger", "deleted", http.StatusForbidden},
		{"owner", "deleted", http.StatusForbidden},
	} {
		if got := comment(c.who, c.doc); got != c.want {
			t.Errorf("%s commenting on the %s doc: %d, want %d", c.who, c.doc, got, c.want)
		}
	}
	if n := comments("stranger"); n != 0 {
		t.Errorf("someone who can't read the docs left %d comments", n)
	}
	if n := comments("owner"); n != 2 {
		t.Errorf("the owner has %d comments, want 2 (none on the deleted doc)", n)
	}
}
