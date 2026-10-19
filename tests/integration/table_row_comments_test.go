//go:build integration
// +build integration

package integration_test

// Comments on a table's rows (tables A8, design/tables-vs-airtable/PROGRAM.md
// §6; business/TableComments) against Postgres 12 with every migration and a
// real graph: threads on a row and on a cell through the threads routes, each
// person deciding by the row's own table; comments kept in thread_comments,
// safe as stored; mentions told only to people who may open the row, its
// writer told of anyone who couldn't be; edits, removals and reactions by the
// rules, an admin's removal in the audit log; a row's comment count with it;
// a row in the trash taking its threads, and its restore bringing them back;
// and the inbox naming a row's notice only while its reader may open it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableRowComments -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	_ "github.com/akashc777/OneCamp/business/TableComments"
	threadController "github.com/akashc777/OneCamp/controllers/Thread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// threadCommentsMigration is migration 348's file for dir, found by name so
// its renumbering at merge keeps the test.
func threadCommentsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_thread_comments."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one thread_comments %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTableRowComments(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// The migration runs down and up again, with every migration after it.
	t.Run("the stored comments' migration runs down and up again on Postgres 12", func(t *testing.T) {
		for _, step := range []string{"down", "up", "down", "up"} {
			if _, err := env.PG.Exec(threadCommentsMigration(t, step)); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
		}
	})

	// The people, in Postgres and in the graph; Ada is a workspace admin.
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
	mention := func(name string) string {
		return `<span data-type="mention" data-id="` + ids[name].String() + `@` + uids[name] + `" data-label="` + name + `">@` + name + `</span>`
	}

	// Two tables of Maya's: Deals, open to the workspace, and Payroll, hers alone.
	makeTable := func(name, visibility string) (*dataTableModel.DataTable, string, uuid.UUID) {
		t.Helper()
		tb, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: name, Visibility: visibility}, actor("maya"))
		if err != nil {
			t.Fatal(err)
		}
		// The Name column every new table starts with names its rows.
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
		r, err := dataTableBusiness.CreateRow(ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{f.Id.String(): "Acme"}}, actor("maya"))
		if err != nil {
			t.Fatal(err)
		}
		return tb, f.Id.String(), r.Id
	}
	deals, nameField, acme := makeTable("Deals", "workspace")
	_, _, payroll := makeTable("Payroll", "private")

	// The routes as router/docsThreads.go mounts them, signed in by a header.
	rt := chi.NewRouter()
	rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, userInfo(r.Header.Get("X-Who")))))
		})
	})
	rt.Get("/threads", threadController.ListThreads)
	rt.Post("/threads", threadController.CreateThread)
	rt.Post("/threads/{thread_id}/replies", threadController.ReplyToThread)
	rt.Post("/threads/{thread_id}/resolve", threadController.ResolveThread)
	rt.Post("/threads/{thread_id}/comments/{comment_id}/edit", threadController.EditThreadComment)
	rt.Post("/threads/{thread_id}/comments/{comment_id}/remove", threadController.RemoveThreadComment)
	rt.Post("/threads/{thread_id}/comments/{comment_id}/react", threadController.ReactToThreadComment)
	call := func(who, method, path string, body any, out any) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		if body == nil {
			raw = nil
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("X-Who", who)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if out != nil && rec.Code == http.StatusOK {
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || json.Unmarshal(env.Data, out) != nil {
				t.Fatalf("%s %s: %s", method, path, rec.Body.String())
			}
		}
		return rec.Code
	}
	type comment struct {
		ID        string          `json:"comment_uuid"`
		Text      string          `json:"comment_text"`
		By        map[string]any  `json:"comment_by"`
		UpdatedAt *string         `json:"comment_updated_at"`
		Reactions json.RawMessage `json:"comment_reactions"`
	}
	type view struct {
		ID       string    `json:"id"`
		Comments []comment `json:"comments"`
		Anchor   struct {
			Kind string          `json:"kind"`
			Meta json.RawMessage `json:"meta"`
		} `json:"anchor"`
		CanModerate bool `json:"can_moderate"`
	}
	start := func(who string, row uuid.UUID, anchor map[string]any, html string) (view, int) {
		var v view
		code := call(who, http.MethodPost, "/threads", map[string]any{
			"resource_kind": "table_row", "resource_uuid": row.String(), "anchor": anchor, "body": html,
		}, &v)
		return v, code
	}
	threadsOf := func(who string, row string) ([]view, int) {
		var out struct {
			Threads []view `json:"threads"`
		}
		code := call(who, http.MethodGet, "/threads?resource_kind=table_row&resource_uuid="+row, nil, &out)
		return out.Threads, code
	}
	noticesOf := func(who, kind string, row uuid.UUID) int {
		t.Helper()
		var n int
		for i := 0; i < 100; i++ { // the notices are written off the request
			integration.Settle(t)
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
				ids[who], kind, row.String()).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return n
	}
	none := map[string]any{"kind": "none"}

	var first view
	t.Run("a comment mentioning someone tells them, if they may open the row", func(t *testing.T) {
		var code int
		first, code = start("maya", acme, none, "<p>"+mention("bo")+" can you take Acme? <script>alert(1)</script></p>")
		if code != http.StatusOK || len(first.Comments) != 1 {
			t.Fatalf("Maya's comment on Acme: %d %+v", code, first)
		}
		if strings.Contains(first.Comments[0].Text, "script") || !strings.Contains(first.Comments[0].Text, `data-type="mention"`) {
			t.Errorf("the stored comment reads %s", first.Comments[0].Text)
		}
		if noticesOf("bo", "table_row.mentioned", acme) != 1 {
			t.Error("Bo, mentioned on a row he may open, wasn't told")
		}
	})

	t.Run("someone mentioned who can't open the row isn't told, and its writer hears so", func(t *testing.T) {
		if _, code := start("maya", payroll, none, "<p>"+mention("bo")+" see this</p>"); code != http.StatusOK {
			t.Fatalf("Maya's comment on her own row: %d", code)
		}
		if n := noticesOf("maya", "table_row.mention.unshared", payroll); n != 1 {
			t.Errorf("Maya heard %d times that Bo couldn't be told", n)
		}
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND resource_uuid = $2`, ids["bo"], payroll.String()).Scan(&n); err != nil || n != 0 {
			t.Errorf("Bo was told %d times about a row he can't open (%v)", n, err)
		}
	})

	t.Run("each person sees a row's threads by its own table, never by an id they spell", func(t *testing.T) {
		if list, code := threadsOf("bo", acme.String()); code != http.StatusOK || len(list) != 1 || list[0].Comments[0].By["user_name"] != "maya" {
			t.Errorf("Bo on Acme: %d %+v", code, list)
		}
		if _, code := threadsOf("bo", payroll.String()); code != http.StatusNotFound {
			t.Errorf("Bo on Maya's private row: %d", code)
		}
		if _, code := start("sam", payroll, none, "<p>hi</p>"); code != http.StatusNotFound {
			t.Errorf("Sam commenting on Maya's private row: %d", code)
		}
		if list, code := threadsOf("ada", payroll.String()); code != http.StatusOK || len(list) != 1 {
			t.Errorf("Ada, an admin, on the private row: %d %d threads", code, len(list))
		}
		if _, code := threadsOf("bo", strings.ToUpper(acme.String())); code != http.StatusNotFound {
			t.Errorf("Acme spelled in capitals: %d", code)
		}
		if _, code := threadsOf("bo", uuid.NewString()); code != http.StatusNotFound {
			t.Errorf("a row that isn't there: %d", code)
		}
	})

	t.Run("a thread on a cell keeps its field", func(t *testing.T) {
		cell, code := start("bo", acme, map[string]any{"kind": "cell", "meta": map[string]any{"field": nameField}}, "<p>Is this the legal name?</p>")
		if code != http.StatusOK || cell.Anchor.Kind != "cell" || !strings.Contains(string(cell.Anchor.Meta), nameField) {
			t.Errorf("a cell's thread: %d %+v", code, cell)
		}
	})

	t.Run("edits, removals and reactions follow the rules, and an admin's removal is recorded", func(t *testing.T) {
		c := first.Comments[0].ID
		edit := "/threads/" + first.ID + "/comments/" + c + "/edit"
		if code := call("bo", http.MethodPost, edit, map[string]any{"body": "<p>mine</p>"}, nil); code != http.StatusForbidden {
			t.Errorf("Bo editing Maya's comment: %d", code)
		}
		var edited comment
		if code := call("maya", http.MethodPost, edit, map[string]any{"body": "<p>Bo, can you take Acme?</p>"}, &edited); code != http.StatusOK || edited.UpdatedAt == nil {
			t.Errorf("Maya editing her comment: %d %+v", code, edited)
		}
		if code := call("bo", http.MethodPost, "/threads/"+first.ID+"/comments/"+c+"/react", map[string]any{"emoji_id": "thumbs_up"}, nil); code != http.StatusOK {
			t.Errorf("Bo reacting: %d", code)
		}
		var reply comment
		if code := call("bo", http.MethodPost, "/threads/"+first.ID+"/replies", map[string]any{"body": "<p>On it</p>"}, &reply); code != http.StatusOK {
			t.Fatalf("Bo replying: %d", code)
		}
		if noticesOf("maya", "thread.replied", acme) != 1 {
			t.Error("Maya, who started the thread, wasn't told of Bo's reply")
		}
		if code := call("sam", http.MethodPost, "/threads/"+first.ID+"/comments/"+reply.ID+"/remove", nil, nil); code != http.StatusForbidden {
			t.Errorf("Sam removing Bo's reply: %d", code)
		}
		if code := call("ada", http.MethodPost, "/threads/"+first.ID+"/comments/"+reply.ID+"/remove", nil, nil); code != http.StatusOK {
			t.Errorf("Ada, an admin, removing Bo's reply: %d", code)
		}
		var n int
		for i := 0; i < 50 && n == 0; i++ { // the audit entry is written off the request
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE action = 'thread.comment.removed' AND metadata::jsonb ->> 'comment_id' = $1`, reply.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n != 1 {
			t.Errorf("an admin's removal is recorded %d times, want once", n)
		}
		list, _ := threadsOf("maya", acme.String())
		if len(list) == 0 || len(list[0].Comments) != 1 || !strings.Contains(string(list[0].Comments[0].Reactions), "thumbs_up") {
			t.Errorf("after the removal: %+v", list)
		}
	})

	t.Run("a row's comment count comes with it, for members", func(t *testing.T) {
		rows, err := dataTableBusiness.ListRows(ctx, deals.Id, actor("bo"), 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		dataTableBusiness.WithCommentCounts(ctx, rows)
		if len(rows) != 1 || rows[0].Comments != 2 { // Maya's, and Bo's on the cell
			t.Errorf("Acme's count: %+v", rows)
		}
	})

	t.Run("a row in the trash takes its threads, and its restore brings them back", func(t *testing.T) {
		if _, err := dataTableBusiness.Batch(ctx, deals.Id, dataTableBusiness.BatchInput{Delete: []uuid.UUID{acme}}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if _, code := threadsOf("bo", acme.String()); code != http.StatusGone {
			t.Errorf("a deleted row's threads: %d", code)
		}
		if _, code := start("bo", acme, none, "<p>still here?</p>"); code != http.StatusGone {
			t.Errorf("a comment on a deleted row: %d", code)
		}
		if _, err := dataTableBusiness.RestoreFromTrash(ctx, deals.Id, dataTableBusiness.RestoreInput{Rows: []uuid.UUID{acme}}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if list, code := threadsOf("bo", acme.String()); code != http.StatusOK || len(list) != 2 {
			t.Errorf("the restored row's threads: %d, %d threads", code, len(list))
		}
	})

	t.Run("the inbox names a row's notice only while its reader may open it", func(t *testing.T) {
		item := func() *dgraphModels.NoticeItem {
			page := activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, ids["bo"].String(), time.Now().Add(time.Hour), 20)
			for _, a := range page.Activities {
				if a.Notice != nil && a.Notice.Kind == "table_row.mentioned" && a.Notice.ResourceUUID == acme.String() {
					return a.Notice
				}
			}
			return nil
		}
		n := item()
		if n == nil || !n.CanOpen || n.Title != "Acme in Deals" {
			t.Fatalf("Bo's notice: %+v", n)
		}
		private := "private"
		if _, _, err := dataTableBusiness.UpdateTable(ctx, deals.Id, dataTableBusiness.TablePatch{Visibility: &private}, actor("maya")); err != nil {
			t.Fatal(err)
		}
		if n := item(); n == nil || n.CanOpen || n.Title != "" {
			t.Errorf("Bo's notice once the table is private: %+v", n)
		}
	})
}
