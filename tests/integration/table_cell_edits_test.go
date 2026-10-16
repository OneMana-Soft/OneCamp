//go:build integration
// +build integration

package integration_test

// Two people editing different cells of one row, against Postgres 12 with
// every migration (TAB-BUG-1).
//
// The web saved a cell edit by sending the whole row as its grid last read
// it, and the server stored that copy: Amara set Status to "Won", Bo typed a
// note into the same row a moment later from a copy that still said "Open",
// and Amara's "Won" was gone. A cell edit now goes through
// POST /tables/{id}/rows/{rowId}/patch, which sets the cells it names and
// leaves the rest of the row as it is when it's written.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableCellEdits -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// tableWorld is a workspace for the table tests: Postgres with every
// migration, people by name, and a table they share.
type tableWorld struct {
	t      *testing.T
	ctx    context.Context
	env    *integration.Env
	people map[string]dataTableBusiness.Actor
}

func newTableWorld(t *testing.T, names ...string) *tableWorld {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.StubMqttClient(t)
	w := &tableWorld{t: t, ctx: ctx, env: env, people: map[string]dataTableBusiness.Actor{}}
	for _, name := range names {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		w.people[name] = dataTableBusiness.Actor{UserID: id}
	}
	return w
}

// ptr is v's address, for the fields a patch names.
func ptr[T any](v T) *T { return &v }

// table makes a workspace table owned by who.
func (w *tableWorld) table(who, name string) *dataTableModel.DataTable {
	w.t.Helper()
	tb, err := dataTableBusiness.CreateTable(w.ctx, dataTableBusiness.TableInput{Name: name, Visibility: "workspace"}, w.people[who])
	if err != nil {
		w.t.Fatalf("create %s: %v", name, err)
	}
	return tb
}

// field adds a field to tb as its owner, and returns its id.
func (w *tableWorld) field(who string, tb *dataTableModel.DataTable, name, typ string, cfg map[string]interface{}) string {
	w.t.Helper()
	f, err := dataTableBusiness.CreateField(w.ctx, tb.Id, dataTableBusiness.FieldInput{Name: name, Type: typ, Config: cfg}, w.people[who])
	if err != nil {
		w.t.Fatalf("create field %s: %v", name, err)
	}
	return f.Id.String()
}

// stored is a row's cells as the database holds them.
func (w *tableWorld) stored(tb *dataTableModel.DataTable, row uuid.UUID) map[string]interface{} {
	w.t.Helper()
	r, err := dataTableModel.GetRowByID(w.ctx, tb.Id, row)
	if err != nil || r == nil {
		w.t.Fatalf("read row %s: %v", row, err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(r.Values), &v); err != nil {
		w.t.Fatalf("row %s values: %v", row, err)
	}
	return v
}

// asMember serves h as who would reach it: signed in, with their account in
// the request's context, as the auth middleware puts it.
func (w *tableWorld) asMember(who string, method, path string, h http.HandlerFunc) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			info := userModels.UserInfo{UserPostgresInfo: userModels.User{Id: w.people[who].UserID}}
			next.ServeHTTP(rw, req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, info)))
		})
	})
	r.MethodFunc(method, path, h)
	return r
}

func TestTableCellEdits(t *testing.T) {
	w := newTableWorld(t, "amara", "bo")
	deals := w.table("amara", "Deals")
	status := w.field("amara", deals, "Status", dataTableModel.FieldSelect,
		map[string]interface{}{"options": []map[string]interface{}{{"label": "Open"}, {"label": "Won"}}})
	notes := w.field("amara", deals, "Notes", dataTableModel.FieldText, nil)
	newRow := func(t *testing.T) uuid.UUID {
		t.Helper()
		r, err := dataTableBusiness.CreateRow(w.ctx, deals.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{status: "Open", notes: ""}}, w.people["amara"])
		if err != nil {
			t.Fatalf("create row: %v", err)
		}
		return r.Id
	}

	t.Run("Amara's status and Bo's note both stay", func(t *testing.T) {
		row := newRow(t)
		if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, map[string]interface{}{status: "Won"}, w.people["amara"]); err != nil {
			t.Fatalf("Amara's edit: %v", err)
		}
		// Bo's grid still says "Open"; his edit names only the note.
		if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, map[string]interface{}{notes: "Call back Friday"}, w.people["bo"]); err != nil {
			t.Fatalf("Bo's edit: %v", err)
		}
		if v := w.stored(deals, row); v[status] != "Won" || v[notes] != "Call back Friday" {
			t.Errorf("after both edits the row holds %v, want Status Won and Bo's note", v)
		}
	})

	t.Run("edits at the same moment keep each other", func(t *testing.T) {
		row := newRow(t)
		const rounds = 25
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 2*rounds)
		edit := func(who, field, prefix string) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, map[string]interface{}{field: fmt.Sprintf("%s %d", prefix, i)}, w.people[who]); err != nil {
					errs <- err
				}
			}
		}
		wg.Add(2)
		go edit("amara", status, "status")
		go edit("bo", notes, "note")
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("an edit failed: %v", err)
		}
		last := fmt.Sprintf(" %d", rounds-1)
		if v := w.stored(deals, row); v[status] != "status"+last || v[notes] != "note"+last {
			t.Errorf("after both people's last edits the row holds %v", v)
		}
	})

	t.Run("a null clears one cell", func(t *testing.T) {
		row := newRow(t)
		if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, map[string]interface{}{notes: "Draft"}, w.people["bo"]); err != nil {
			t.Fatal(err)
		}
		if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, map[string]interface{}{notes: nil}, w.people["bo"]); err != nil {
			t.Fatalf("clearing: %v", err)
		}
		if v := w.stored(deals, row); v[notes] != nil || v[status] != "Open" {
			t.Errorf("after clearing the note the row holds %v", v)
		}
	})

	// It touched the row, and told its readers, webhooks and AI columns of a
	// change, for nothing.
	t.Run("a patch naming no cell the row stores is refused, and changes nothing", func(t *testing.T) {
		row := newRow(t)
		shout := w.field("amara", deals, "Shout", dataTableModel.FieldFormula, map[string]interface{}{"formula": "UPPER({Notes})"})
		var before, after string
		if err := w.env.PG.QueryRow(`SELECT updated_at::text FROM data_table_rows WHERE id = $1`, row).Scan(&before); err != nil {
			t.Fatal(err)
		}
		for _, values := range []map[string]interface{}{{}, nil, {shout: "LOUD"}} {
			if _, err := dataTableBusiness.PatchRow(w.ctx, deals.Id, row, values, w.people["bo"]); err == nil || err.Error() != "name the cells to change" {
				t.Errorf("a patch of %v answered %v", values, err)
			}
		}
		if err := w.env.PG.QueryRow(`SELECT updated_at::text FROM data_table_rows WHERE id = $1`, row).Scan(&after); err != nil || after != before {
			t.Errorf("the row was touched: %s, was %s (%v)", after, before, err)
		}
		rec := httptest.NewRecorder()
		w.asMember("bo", http.MethodPost, "/tables/{id}/rows/{rowId}/patch", dataTableController.PatchRow).
			ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tables/"+deals.Id.String()+"/rows/"+row.String()+"/patch", bytes.NewBufferString(`{"values":{}}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("an empty patch over HTTP answered %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("over HTTP: the patch route sets one cell, and a deleted row is a 404", func(t *testing.T) {
		row := newRow(t)
		const path = "/tables/{id}/rows/{rowId}/patch"
		h := w.asMember("bo", http.MethodPost, path, dataTableController.PatchRow)
		send := func(rowID uuid.UUID, body string) (int, map[string]interface{}) {
			t.Helper()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/tables/"+deals.Id.String()+"/rows/"+rowID.String()+"/patch", bytes.NewBufferString(body))
			h.ServeHTTP(rec, req)
			var out map[string]interface{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
		code, out := send(row, fmt.Sprintf(`{"values": {%q: "Won"}}`, status))
		if code != http.StatusOK {
			t.Fatalf("patch answered %d %v", code, out)
		}
		data, _ := out["data"].(map[string]interface{})
		var cells map[string]interface{}
		_ = json.Unmarshal([]byte(fmt.Sprint(data["values"])), &cells)
		if cells[status] != "Won" || data["id"] != row.String() {
			t.Errorf("the patch answered row %v with %v", data["id"], cells)
		}
		if v := w.stored(deals, row); v[status] != "Won" || v[notes] != "" {
			t.Errorf("after the patch the row holds %v", v)
		}

		if err := dataTableBusiness.DeleteRow(w.ctx, deals.Id, row, w.people["bo"]); err != nil {
			t.Fatal(err)
		}
		if code, out := send(row, fmt.Sprintf(`{"values": {%q: "Open"}}`, status)); code != http.StatusNotFound || out["msg"] != "row not found" {
			t.Errorf("patching a deleted row answered %d %v, want 404 row not found", code, out)
		}
		if code, _ := send(uuid.New(), fmt.Sprintf(`{"values": {%q: "Open"}}`, status)); code != http.StatusNotFound {
			t.Errorf("patching a row that never was answered %d, want 404", code)
		}
		// So do the whole-row update and the delete, which said "failed to".
		if _, err := dataTableBusiness.UpdateRow(w.ctx, deals.Id, row, dataTableBusiness.RowInput{Values: map[string]interface{}{status: "Open"}}, w.people["bo"]); !dataTableBusiness.IsRowNotFound(err) {
			t.Errorf("updating a deleted row answered %v, want row not found", err)
		}
		if err := dataTableBusiness.DeleteRow(w.ctx, deals.Id, row, w.people["bo"]); !dataTableBusiness.IsRowNotFound(err) {
			t.Errorf("deleting a deleted row answered %v, want row not found", err)
		}
	})
}
