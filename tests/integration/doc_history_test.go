//go:build integration
// +build integration

package integration_test

// Docs H1 against real Postgres 12, Dgraph, Redis and MinIO: a table row's
// page (a hosted doc, D-18) keeps its history, and its history routes open it
// for its host, as the row page's own read does; a path that didn't open
// hosted docs still refuses it. A long history is read a page at a time.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestDocHistory' -v

import (
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

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	snapshotBusiness "github.com/akashc777/OneCamp/business/Snapshot"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocHistoryOfARowsPage(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	startMinio(t)

	// Maya keeps a private table; Sam isn't in it.
	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range []string{"maya", "sam"} {
		id := uuid.New()
		ids[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	userInfo := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = ids[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: ids[name].String(), UserName: name}
		return u
	}
	maya := dataTableBusiness.Actor{UserID: ids["maya"]}
	tb, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Payroll", Visibility: "private"}, maya)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := dataTableModel.ListFields(ctx, tb.Id)
	if err != nil || len(fields) == 0 {
		t.Fatalf("columns: %v", err)
	}
	row, err := dataTableBusiness.CreateRow(ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{fields[0].Id.String(): "June"}}, maya)
	if err != nil {
		t.Fatal(err)
	}

	rt := chi.NewRouter()
	rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, userInfo(r.Header.Get("X-Who")))))
		})
	})
	rt.Post("/tables/{id}/rows/{rowId}/page", dataTableController.EnsureRowPage)
	rt.Get("/doc/versions", docController.ListDocVersions)
	send := func(who, method, path string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		req.Header.Set("X-Who", who)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := send("maya", http.MethodPost, "/tables/"+tb.Id.String()+"/rows/"+row.Id.String()+"/page")
	var made struct {
		Data struct {
			Doc string `json:"doc_uuid"`
		} `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &made) != nil || made.Data.Doc == "" {
		t.Fatalf("the row's page: %d %s", code, body)
	}
	page := made.Data.Doc

	// A history longer than a page, kept as the save path keeps it.
	start := time.Now().Add(-48 * time.Hour)
	for i := 0; i < snapshotBusiness.ListPage+5; i++ {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO doc_snapshots (id, doc_uuid, object_key, reason, kind, body_bytes, stored_bytes, created_at) VALUES ($1, $2, $3, 'idle', 'doc', 10, 10, $4)`,
			id, page, "h1/"+id.String(), start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("a version: %v", err)
		}
	}

	type listed struct {
		Data struct {
			Versions []snapshotBusiness.Version `json:"versions"`
			Next     string                     `json:"next"`
		} `json:"data"`
	}
	t.Run("the history route opens a row's page for whoever its table lets edit, a page at a time", func(t *testing.T) {
		code, body := send("maya", http.MethodGet, "/doc/versions?doc_uuid="+page)
		var first listed
		if code != http.StatusOK || json.Unmarshal([]byte(body), &first) != nil || len(first.Data.Versions) != snapshotBusiness.ListPage || first.Data.Next == "" {
			t.Fatalf("first page: %d %.300s", code, body)
		}
		code, body = send("maya", http.MethodGet, "/doc/versions?doc_uuid="+page+"&before="+first.Data.Next)
		var rest listed
		if code != http.StatusOK || json.Unmarshal([]byte(body), &rest) != nil || len(rest.Data.Versions) != 5 || rest.Data.Next != "" {
			t.Fatalf("second page: %d %.300s", code, body)
		}
	})
	t.Run("someone the table doesn't let in gets the answer a missing doc gets", func(t *testing.T) {
		if code, body := send("sam", http.MethodGet, "/doc/versions?doc_uuid="+page); code != http.StatusNotFound {
			t.Fatalf("Sam: %d %s", code, body)
		}
	})
	t.Run("a path that didn't open hosted docs still refuses the row's page", func(t *testing.T) {
		who := accessBusiness.Subject{UserUID: uids["maya"], UserUUID: ids["maya"].String()}
		if _, err := snapshotBusiness.Snapshots.List(ctx, who, accessBusiness.KindDoc, page, time.Time{}); err == nil {
			t.Fatal("the versions of a row's page were listed on a path that never opened hosted docs")
		}
	})
	integration.Settle(t)
}
