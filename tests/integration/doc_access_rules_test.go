//go:build integration
// +build integration

package integration_test

// Who may edit a private doc, change who sees it, open it in the editor and
// record a view of it, asked of a real graph: its creator, someone it's
// shared with to edit, someone it's shared with to read, and someone else.
// The creator is recognised whether the query returns the creator to
// everyone or (the edit-access query) only to the creator.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocAccessRules -v

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
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocAccessRules(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// An edit indexes the doc in the background; give it somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]uuid.UUID{"owner": uuid.New(), "editor": uuid.New(), "viewer": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	doc := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "plans",
		"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
		"doc_created_by":    map[string]any{"uid": "_:owner"},
		"doc_editing_users": []map[string]any{{"uid": "_:owner"}, {"uid": "_:editor"}},
		"doc_reading_users": []map[string]any{{"uid": "_:viewer"}}})
	uids := dg.Mutate(t, nodes)

	as := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	call := func(who string, handler http.HandlerFunc, body any) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, as(who)))
		handler(rec, req)
		return rec.Code
	}
	title, public, private := "plans v2", false, true

	for _, c := range []struct {
		who                               string
		edit, collab, changeWhoSees, view bool
	}{
		{"owner", true, true, true, true},
		{"editor", true, true, false, true},
		{"viewer", false, false, false, true},
		{"stranger", false, false, false, false},
	} {
		code := func(ok bool) int {
			if ok {
				return http.StatusOK
			}
			return http.StatusForbidden
		}
		if got := call(c.who, docController.UpdateDocBody, docAdapter.InputUpdateDoc{DocId: doc, Title: &title}); got != code(c.edit) {
			t.Errorf("%s renaming the doc: %d, want %d", c.who, got, code(c.edit))
		}
		if got := call(c.who, docController.DocCollabAuthorize, docAdapter.InputUpdateDoc{DocId: doc}); got != code(c.collab) {
			t.Errorf("%s opening it in the editor: %d, want %d", c.who, got, code(c.collab))
		}
		if ok, err := docBusiness.CheckUserDocEditAccess(ctx, doc, uids[c.who]); err != nil || ok != c.edit {
			t.Errorf("%s: CheckUserDocEditAccess = %v (%v), want %v", c.who, ok, err, c.edit)
		}
		if c.edit {
			// Making it public, then private again, is its owner's alone.
			if got := call(c.who, docController.UpdateDocBody, docAdapter.InputUpdateDoc{DocId: doc, IsPrivate: &public}); got != code(c.changeWhoSees) {
				t.Errorf("%s making the doc public: %d, want %d", c.who, got, code(c.changeWhoSees))
			}
			if got := call(c.who, docController.UpdateDocBody, docAdapter.InputUpdateDoc{DocId: doc, IsPrivate: &private}); got != code(c.changeWhoSees) {
				t.Errorf("%s making the doc private: %d, want %d", c.who, got, code(c.changeWhoSees))
			}
		}
		err := docBusiness.RecordDocView(ctx, doc, uids[c.who], people[c.who].String())
		if (err == nil) != c.view {
			t.Errorf("%s recording a view: %v, want allowed=%v", c.who, err, c.view)
		}
	}
}
