//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docController "github.com/akashc777/OneCamp/controllers/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The docs quick wins' server side (docs PROGRAM A0): a doc whose creator was
// removed is refused, never a fault; a deleted doc's page says who may restore
// it; and an update to a uuid with no doc makes no doc.
func TestDocQuickWins(t *testing.T) {
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

	people := map[string]uuid.UUID{"owner": uuid.New(), "editor": uuid.New(), "viewer": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	// orphan: its creator was removed (no doc_created_by). trashed: in the trash.
	orphan, trashed := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:orphan", "dgraph.type": "Doc", "doc_uuid": orphan, "doc_title": "notes",
			"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_editing_users": []map[string]any{{"uid": "_:editor"}},
			"doc_reading_users": []map[string]any{{"uid": "_:viewer"}}},
		map[string]any{"uid": "_:trashed", "dgraph.type": "Doc", "doc_uuid": trashed, "doc_title": "old plan",
			"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "2026-10-01T09:00:00Z",
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
	call := func(who string, handler http.HandlerFunc, body any, docParam string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("doc_uuid", docParam)
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, as(who)), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
		return rec
	}

	t.Run("deleting a doc whose creator was removed is refused, never a fault", func(t *testing.T) {
		for _, who := range []string{"editor", "viewer", "stranger"} {
			rec := call(who, docController.DeleteDocTree, map[string]string{"doc_uuid": orphan}, "")
			if rec.Code >= 500 {
				t.Errorf("%s: %d %s", who, rec.Code, rec.Body)
			}
			if who != "editor" && rec.Code < 400 {
				t.Errorf("%s deleted a doc they can't change: %d", who, rec.Code)
			}
		}
	})

	t.Run("a deleted doc's page says who may restore it", func(t *testing.T) {
		for _, c := range []struct {
			who        string
			code       int
			canRestore bool
		}{{"owner", http.StatusGone, true}, {"editor", http.StatusGone, true}, {"viewer", http.StatusGone, false}, {"stranger", http.StatusNotFound, false}} {
			rec := call(c.who, docController.GetDocPage, nil, trashed)
			if rec.Code != c.code {
				t.Fatalf("%s: %d, want %d", c.who, rec.Code, c.code)
			}
			var body struct {
				CanRestore bool `json:"can_restore"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.CanRestore != c.canRestore {
				t.Errorf("%s: can_restore %v, want %v", c.who, body.CanRestore, c.canRestore)
			}
		}
	})

	t.Run("the share dialog's read answers editors once, and nobody else", func(t *testing.T) {
		live := uuid.NewString()
		ids := dg.Mutate(t, []map[string]any{{"uid": "_:live", "dgraph.type": "Doc", "doc_uuid": live, "doc_title": "live",
			"doc_private": true, "doc_public_comment": false, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_by":    map[string]any{"uid": uids["owner"]},
			"doc_editing_users": []map[string]any{{"uid": uids["owner"]}, {"uid": uids["editor"]}},
			"doc_reading_users": []map[string]any{{"uid": uids["viewer"]}}}})
		if ids["live"] == "" {
			t.Fatal("no doc made")
		}
		get := func(who string) int {
			rctx := chi.NewRouteContext()
			c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, as(who)), chi.RouteCtxKey, rctx)
			rec := httptest.NewRecorder()
			docController.GetDocPermissions(rec, httptest.NewRequest(http.MethodGet, "/?doc_uuid="+live, nil).WithContext(c))
			return rec.Code
		}
		for who, want := range map[string]int{"owner": http.StatusOK, "editor": http.StatusOK, "viewer": http.StatusForbidden} {
			if got := get(who); got != want {
				t.Errorf("%s: %d, want %d", who, got, want)
			}
		}
	})

	t.Run("an update to a uuid with no doc makes no doc", func(t *testing.T) {
		ghost := uuid.NewString()
		_, err := docDomain.CreateOrUpdateDgraphDoc(ctx, &dgraphStruct.DgraphDoc{Uid: "uid(doc)", Uuid: ghost, Title: "ghost"})
		if !errors.Is(err, docDomain.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if _, err := docDomain.GetDocDeletion(ctx, ghost); !errors.Is(err, docDomain.ErrNotFound) {
			t.Fatalf("a doc was made for %s: %v", ghost, err)
		}
	})
}
