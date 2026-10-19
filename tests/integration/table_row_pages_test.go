//go:build integration
// +build integration

package integration_test

// A table's rows as pages (tables T2; docs D-18, amended) against Postgres 12
// with every migration and a real graph: a row's page made once however many
// ask at once; read with the row, at the row's level never above edit; joined
// live by the docs' session by its row, and its files decided by its row; the
// docs' own paths refusing it, telling someone the row is open to where it is
// and a stranger exactly what a doc that isn't there tells them, whatever a
// request carries; out of every docs list and the docs index; following its
// row into the trash and back, and its table out of the workspace and back.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableRowPages -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// rowPagesMigration is migration 307's file for dir, found by name so its
// renumbering at merge keeps the test.
func rowPagesMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_table_row_pages."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one table_row_pages %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTableRowPages(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Search answers every write, and remembers what each was about.
	var searchMu sync.Mutex
	var searched []string
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		searchMu.Lock()
		searched = append(searched, r.URL.Path+" "+string(body))
		searchMu.Unlock()
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	searchedFor := func(id string) bool {
		searchMu.Lock()
		defer searchMu.Unlock()
		for _, s := range searched {
			if strings.Contains(s, id) {
				return true
			}
		}
		return false
	}

	t.Run("the pages' migration runs down and up again on Postgres 12", func(t *testing.T) {
		for _, step := range []string{"down", "up", "down", "up"} {
			if _, err := env.PG.Exec(rowPagesMigration(t, step)); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
	})

	// The people; Ada is a workspace admin.
	names := []string{"maya", "bo", "sam", "ada"}
	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, name := range names {
		id := uuid.New()
		ids[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ($1)`, ids["ada"].String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, nodes)
	userInfo := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id, u.UserPostgresInfo.IsAdmin = ids[name], name == "ada"
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: ids[name].String(), UserName: name}
		return u
	}
	actor := func(name string) dataTableBusiness.Actor {
		return dataTableBusiness.Actor{UserID: ids[name], IsAdmin: name == "ada"}
	}

	// Maya's tables: Deals, open to the workspace, and Payroll, hers alone.
	makeTable := func(name, visibility, row string) (*dataTableModel.DataTable, uuid.UUID) {
		t.Helper()
		tb, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: name, Visibility: visibility}, actor("maya"))
		if err != nil {
			t.Fatal(err)
		}
		fields, err := dataTableModel.ListFields(ctx, tb.Id)
		if err != nil || len(fields) == 0 {
			t.Fatalf("%s's first column: %v", name, err)
		}
		f := fields[0]
		for _, x := range fields {
			if x.Primary {
				f = x
			}
		}
		r, err := dataTableBusiness.CreateRow(ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{f.Id.String(): row}}, actor("maya"))
		if err != nil {
			t.Fatal(err)
		}
		return tb, r.Id
	}
	deals, acme := makeTable("Deals", "workspace", "Acme")
	payroll, june := makeTable("Payroll", "private", "June")

	// The routes as router/tablesRowPages.go and the docs' routers mount them,
	// signed in by a header.
	rt := chi.NewRouter()
	rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, userInfo(r.Header.Get("X-Who")))))
		})
	})
	rt.Get("/tables/{id}/rows/{rowId}/page", dataTableController.GetRowPage)
	rt.Post("/tables/{id}/rows/{rowId}/page", dataTableController.EnsureRowPage)
	rt.Get("/doc/getDocInfo/{doc_uuid}", docController.GetDocPage)
	rt.Get("/doc/getCommentList/{doc_uuid}", docController.GetAllCommentList)
	rt.Get("/doc/list", docController.GetDocList)
	rt.Get("/doc/children", docController.GetDocChildren)
	rt.Get("/doc/trash", docController.GetDocTrash)
	rt.Post("/docColab/session", docController.DocCollabSession)
	type answer struct {
		code int
		body string
	}
	send := func(who, method, path string, body any, header map[string]string) answer {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("X-Who", who)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return answer{rec.Code, rec.Body.String()}
	}
	data := func(t *testing.T, a answer, out any) {
		t.Helper()
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(a.body), &env); err != nil || json.Unmarshal(env.Data, out) != nil {
			t.Fatalf("%d %s", a.code, a.body)
		}
	}
	pagePath := func(table, row uuid.UUID) string {
		return "/tables/" + table.String() + "/rows/" + row.String() + "/page"
	}
	type made struct {
		Doc  string `json:"doc_uuid"`
		Made bool   `json:"made"`
	}
	type page struct {
		Doc   string `json:"doc_uuid"`
		Body  string `json:"body"`
		Level string `json:"level"`
	}
	session := func(who, doc string) (answer, map[string]any) {
		a := send(who, http.MethodPost, "/docColab/session", map[string]string{"doc_uuid": doc}, nil)
		var s map[string]any
		_ = json.Unmarshal([]byte(a.body), &s)
		return a, s
	}

	var dealsPage, payrollPage string
	t.Run("a row's page is made once, however many ask at once", func(t *testing.T) {
		const asks = 8
		got := make([]made, asks)
		codes := make([]int, asks)
		var wg sync.WaitGroup
		for i := 0; i < asks; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				a := send("bo", http.MethodPost, pagePath(deals.Id, acme), nil, nil)
				codes[i] = a.code
				if a.code == http.StatusOK {
					var env struct {
						Data made `json:"data"`
					}
					_ = json.Unmarshal([]byte(a.body), &env)
					got[i] = env.Data
				}
			}(i)
		}
		wg.Wait()
		makes := 0
		for i := range got {
			if codes[i] != http.StatusOK || got[i].Doc == "" || got[i].Doc != got[0].Doc {
				t.Fatalf("ask %d: %d %+v (first %+v)", i, codes[i], got[i], got[0])
			}
			if got[i].Made {
				makes++
			}
		}
		if makes != 1 {
			t.Errorf("%d asks said they made the page; want one", makes)
		}
		dealsPage = got[0].Doc
		var stored uuid.NullUUID
		if err := env.PG.QueryRow(`SELECT doc_uuid FROM data_table_rows WHERE id = $1`, acme).Scan(&stored); err != nil || !stored.Valid || stored.UUID.String() != dealsPage {
			t.Errorf("the row's link: %v %v, want %s", stored, err, dealsPage)
		}
		var hosted struct {
			Docs []struct {
				UUID string `json:"doc_uuid"`
			} `json:"docs"`
		}
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query q($row: string) { docs(func: eq(doc_host_uuid, $row)) { doc_uuid } }`, map[string]string{"$row": acme.String()})
		if err != nil || json.Unmarshal(resp.Json, &hosted) != nil || len(hosted.Docs) != 1 || hosted.Docs[0].UUID != dealsPage {
			t.Errorf("the graph holds %s for the row (%v); want one doc, %s", resp.GetJson(), err, dealsPage)
		}
		a := send("maya", http.MethodPost, pagePath(payroll.Id, june), nil, nil)
		var m made
		data(t, a, &m)
		payrollPage = m.Doc
		if !m.Made || payrollPage == "" || payrollPage == dealsPage {
			t.Fatalf("Payroll's page: %+v", m)
		}
	})

	t.Run("its page reads with the row, at the row's level never above edit", func(t *testing.T) {
		for who, want := range map[string]string{"bo": "edit", "maya": "edit", "ada": "edit"} {
			var p page
			data(t, send(who, http.MethodGet, pagePath(deals.Id, acme), nil, nil), &p)
			if p.Doc != dealsPage || p.Level != want {
				t.Errorf("%s reads %+v; want %s at %s", who, p, dealsPage, want)
			}
		}
		if a := send("sam", http.MethodGet, pagePath(payroll.Id, june), nil, nil); a.code != http.StatusNotFound {
			t.Errorf("Sam reads Payroll's row: %d %s", a.code, a.body)
		}
		if a := send("sam", http.MethodPost, pagePath(payroll.Id, june), nil, nil); a.code != http.StatusNotFound {
			t.Errorf("Sam makes Payroll's page: %d %s", a.code, a.body)
		}
		// A row through another table's address is no row.
		if a := send("maya", http.MethodGet, pagePath(payroll.Id, acme), nil, nil); a.code != http.StatusNotFound {
			t.Errorf("Acme through Payroll: %d %s", a.code, a.body)
		}
	})

	t.Run("the live session opens it by its row, and its files are its row's", func(t *testing.T) {
		a, s := session("bo", dealsPage)
		if a.code != http.StatusOK || s["level"] != "edit" || s["canEdit"] != true || s["locked"] != false {
			t.Errorf("Bo's session: %d %s", a.code, a.body)
		}
		if a, _ := session("sam", payrollPage); a.code != http.StatusForbidden {
			t.Errorf("Sam's session on Payroll's page: %d %s", a.code, a.body)
		}
		place := fileAccessBusiness.Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: dealsPage}
		if err := fileAccessBusiness.MayAdd(ctx, ids["bo"].String(), place); err != nil {
			t.Errorf("Bo adds an image to the page: %v", err)
		}
		if err := fileAccessBusiness.MayRead(ctx, ids["bo"].String(), place); err != nil {
			t.Errorf("Bo reads the page's image: %v", err)
		}
		theirs := fileAccessBusiness.Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: payrollPage}
		if err := fileAccessBusiness.MayAdd(ctx, ids["sam"].String(), theirs); err == nil {
			t.Error("Sam added an image to Payroll's page")
		}
		if err := fileAccessBusiness.MayRead(ctx, ids["sam"].String(), theirs); err == nil {
			t.Error("Sam read Payroll's page's image")
		}
		// An agent never opens a row's page, whoever it acts for.
		d, err := accessBusiness.Rules.Resolve(docBusiness.WithHostedOpen(ctx),
			accessBusiness.Subject{UserUID: uids["maya"], UserUUID: ids["maya"].String(), AgentUUID: uuid.NewString()},
			accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: dealsPage})
		if err != nil || d.Level != accessBusiness.LevelNone || !d.Hosted {
			t.Errorf("an agent of Maya's: %+v, %v", d, err)
		}
	})

	t.Run("docs' own paths refuse it, whatever a request carries", func(t *testing.T) {
		for _, header := range []map[string]string{nil, {"X-Open-Hosted": "1", "X-Hosted-Open": "true"}} {
			for _, path := range []string{"/doc/getDocInfo/" + dealsPage, "/doc/getDocInfo/" + dealsPage + "?open_hosted=1", "/doc/getCommentList/" + dealsPage} {
				a := send("bo", http.MethodGet, path, nil, header)
				var said map[string]any
				_ = json.Unmarshal([]byte(a.body), &said)
				if a.code != http.StatusForbidden || said["code"] != "hosted" ||
					said["msg"] != "This page belongs to a row of Deals. Change it there." ||
					said["path"] != "/app/tables/"+deals.Id.String()+"?row="+acme.String() {
					t.Errorf("Bo asks %s with %v: %d %s", path, header, a.code, a.body)
				}
			}
		}
	})

	t.Run("a stranger is told exactly what a doc that isn't there tells them", func(t *testing.T) {
		nowhere := uuid.NewString()
		for _, path := range []string{"/doc/getDocInfo/", "/doc/getCommentList/"} {
			hosted, missing := send("sam", http.MethodGet, path+payrollPage, nil, nil), send("sam", http.MethodGet, path+nowhere, nil, nil)
			if hosted != missing || hosted.code != http.StatusNotFound {
				t.Errorf("%s: a row's page answers %d %q, a doc that isn't there %d %q", path, hosted.code, hosted.body, missing.code, missing.body)
			}
		}
	})

	t.Run("it is in no docs list, even where it was viewed and starred", func(t *testing.T) {
		// Bo's view of it and star on it, as if they'd been recorded.
		if _, err := env.PG.Exec(`INSERT INTO resource_views (resource_type, resource_uuid, user_uuid) VALUES ('doc', $1, $2)`, dealsPage, ids["bo"].String()); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO user_places (id, user_uuid, kind, resource_kind, resource_uuid, position) VALUES ($1, $2, 'star', 'doc', $3, 'a0')`,
			uuid.New(), ids["bo"].String(), dealsPage); err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, scope := range []string{"recent", "starred", "mine", "shared", "workspace", "archived"} {
			paths = append(paths, "/doc/list?scope="+scope)
		}
		for _, space := range []string{"workspace", "private", "shared"} {
			paths = append(paths, "/doc/children?space="+space)
		}
		paths = append(paths, "/doc/trash")
		// The public API's list (GET /v1/docs) and MCP's list_docs, which
		// list through business/ContentIO, as a token's person.
		for _, who := range []string{"bo", "maya"} {
			for _, scope := range []string{"workspace", "mine", "shared"} {
				listed, _, err := contentIOBusiness.ListDocs(ctx, accessBusiness.Subject{UserUID: uids[who], UserUUID: ids[who].String()}, scope, 100, 0)
				if err != nil {
					t.Fatalf("%s's %s docs through the API: %v", who, scope, err)
				}
				for _, d := range listed {
					if d.UUID == dealsPage || d.UUID == payrollPage {
						t.Errorf("%s's %s docs through the API list a row's page", who, scope)
					}
				}
			}
		}
		for _, who := range []string{"bo", "maya", "ada"} {
			for _, path := range paths {
				for _, header := range []map[string]string{nil, {"X-Open-Hosted": "1"}} {
					a := send(who, http.MethodGet, path, nil, header)
					if a.code != http.StatusOK {
						t.Errorf("%s %s: %d %s", who, path, a.code, a.body)
						continue
					}
					if strings.Contains(a.body, dealsPage) || strings.Contains(a.body, payrollPage) {
						t.Errorf("%s %s lists a row's page: %s", who, path, a.body)
					}
				}
			}
		}
	})

	t.Run("its saves never reach the docs index", func(t *testing.T) {
		body := "<p>Call Acme on Friday about the renewal.</p>"
		if err := docBusiness.UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: dealsPage, Body: &body}); err != nil {
			t.Fatal(err)
		}
		// And as the live editor saves it, through the collaboration
		// service's save, whose hooks a hosted doc runs only if they keep it.
		live := "<p>Call Acme on Friday about the renewal.</p><p>Bring the quote.</p>"
		if saved, err := docBusiness.SaveFromCollab(ctx, docBusiness.CollabSave{DocUUID: dealsPage, HTML: live, YjsState: "c3RhdGU=", Contributors: []string{ids["bo"].String()}}); err != nil || !saved.Changed {
			t.Fatalf("the live save: %+v %v", saved, err)
		}
		body = live
		// A plain doc saved the same way is indexed: the control.
		plain, err := docBusiness.CreateDoc(ctx, &dgraphStruct.DgraphUser{Uid: uids["bo"], Uuid: ids["bo"].String()}, &adapter.InputCreateDoc{DocTitle: "Notes", DocPrivate: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := docBusiness.UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: plain.Uuid, Body: &body}); err != nil {
			t.Fatal(err)
		}
		if left := docBusiness.WaitForSearchWrites(ctx, 20*time.Second); left != 0 {
			t.Fatalf("%d search writes still running", left)
		}
		time.Sleep(200 * time.Millisecond) // the AI index's write is its own goroutine
		if !searchedFor(plain.Uuid) {
			t.Fatal("the plain doc wasn't indexed either: the control failed")
		}
		if searchedFor(dealsPage) {
			t.Error("a row's page reached the search index")
		}
		var p page
		data(t, send("bo", http.MethodGet, pagePath(deals.Id, acme), nil, nil), &p)
		if p.Body != body {
			t.Errorf("the row's page reads %q after the save", p.Body)
		}
	})

	t.Run("who may open its page follows its table", func(t *testing.T) {
		private, workspace := "private", "workspace"
		if _, _, err := dataTableBusiness.UpdateTable(ctx, deals.Id, dataTableBusiness.TablePatch{Visibility: &private}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if a, _ := session("bo", dealsPage); a.code != http.StatusForbidden {
			t.Errorf("Bo's session on a private table's page: %d %s", a.code, a.body)
		}
		if a := send("bo", http.MethodGet, "/doc/getDocInfo/"+dealsPage, nil, nil); a.code != http.StatusNotFound {
			t.Errorf("Bo, a stranger to it now, opens its page: %d %s", a.code, a.body)
		}
		if a, s := session("maya", dealsPage); a.code != http.StatusOK || s["level"] != "edit" {
			t.Errorf("Maya's session on her private table's page: %d %s", a.code, a.body)
		}
		if _, _, err := dataTableBusiness.UpdateTable(ctx, deals.Id, dataTableBusiness.TablePatch{Visibility: &workspace}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if a, _ := session("bo", dealsPage); a.code != http.StatusOK {
			t.Errorf("Bo's session once the table is the workspace's again: %d %s", a.code, a.body)
		}
	})

	t.Run("a row in the trash takes its page, and its restore brings it back", func(t *testing.T) {
		if _, err := dataTableBusiness.Batch(ctx, deals.Id, dataTableBusiness.BatchInput{Delete: []uuid.UUID{acme}}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if a, _ := session("bo", dealsPage); a.code != http.StatusGone {
			t.Errorf("Bo's session on a deleted row's page: %d %s", a.code, a.body)
		}
		if a := send("bo", http.MethodGet, "/doc/getDocInfo/"+dealsPage, nil, nil); a.code != http.StatusGone {
			t.Errorf("Bo opens a deleted row's page: %d %s", a.code, a.body)
		}
		if a := send("bo", http.MethodGet, pagePath(deals.Id, acme), nil, nil); a.code != http.StatusNotFound {
			t.Errorf("a deleted row's page reads: %d %s", a.code, a.body)
		}
		if a := send("bo", http.MethodGet, "/doc/trash", nil, nil); strings.Contains(a.body, dealsPage) {
			t.Errorf("the docs' trash lists the row's page: %s", a.body)
		}
		if _, err := dataTableBusiness.RestoreFromTrash(ctx, deals.Id, dataTableBusiness.RestoreInput{Rows: []uuid.UUID{acme}}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if a, s := session("bo", dealsPage); a.code != http.StatusOK || s["level"] != "edit" {
			t.Errorf("Bo's session on the restored row's page: %d %s", a.code, a.body)
		}
		var p page
		data(t, send("bo", http.MethodGet, pagePath(deals.Id, acme), nil, nil), &p)
		if p.Doc != dealsPage || !strings.Contains(p.Body, "renewal") {
			t.Errorf("the restored row's page: %+v", p)
		}
	})

	t.Run("the grid's rows say which have a page", func(t *testing.T) {
		rows, err := dataTableBusiness.ListRows(ctx, deals.Id, actor("bo"), 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		dataTableBusiness.WithPages(ctx, rows)
		found := false
		for _, r := range rows {
			if r.Id == acme {
				found = r.Page != nil && r.Page.String() == dealsPage
			}
		}
		if !found {
			t.Errorf("Acme's row doesn't carry its page: %+v", rows)
		}
	})
}
