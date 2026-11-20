//go:build integration

package controllers

// Every export of tables is one row in the workspace's admin audit log
// (gsheets-import-02, "Export all tables"): a workbook is the tables' data
// leaving OneCamp, so an admin reads who took which tables, and when. Against
// Postgres 12 with every migration, two members and a table one of them can't
// open: Ben's export of every table writes one row, under the tables' area,
// naming him and exactly the tables his workbook holds (his own and the one
// shared with everyone), never Ana's private table, though the list it is read
// from is the same list; his export of one table writes one row naming that
// table; an export refused, of a table he can't open, writes none.
// An export whose row can't be written still goes ahead (the row is
// best-effort, as the workspace export's), and the log says so in ONE error
// line an admin can find, naming the action and never the tables' data.
// Run: logs/tables-198/expall/integ.sh with PK='./controllers/DataTable/' and the tests' names.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// exportRecord is one export's row in the admin audit log.
type exportRecord struct {
	Actor, Email, Category string
	Tables                 []string
}

// seedMember adds a member of the workspace by name, for both tests' readers.
func seedMember(t *testing.T, ctx context.Context, name string) userModels.User {
	t.Helper()
	u := userModels.User{Id: uuid.New(), EmailID: name + "@example.test"}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())`, u.Id, u.EmailID, name, name); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return u
}

// exportAs answers who's export as the router hands it over: signed in, at
// POST /tables/export for every table (table uuid.Nil), or at
// /tables/{id}/export with the id in the route for one.
func exportAs(who userModels.User, handler http.HandlerFunc, table uuid.UUID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	exportTo(w, who, handler, table, body)
	return w
}

// exportTo is exportAs answered into w: a reader's connection of a test's
// own making.
func exportTo(w http.ResponseWriter, who userModels.User, handler http.HandlerFunc, table uuid.UUID, body string) {
	path := "/tables/export"
	rc := chi.NewRouteContext()
	if table != uuid.Nil {
		path = "/tables/" + table.String() + "/export"
		rc.URLParams.Add("id", table.String())
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	reqCtx = context.WithValue(reqCtx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: who})
	handler(w, req.WithContext(reqCtx))
}

func TestAnExportIsRecordedForItsReaderWithExactlyTheTablesItHolds(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ana, ben := seedMember(t, ctx, "ana"), seedMember(t, ctx, "ben")
	table := func(owner userModels.User, name, visibility string) *model.DataTable {
		tb, err := business.CreateTable(ctx, business.TableInput{Name: name, Visibility: visibility}, business.Actor{UserID: owner.Id})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return tb
	}
	payroll := table(ana, "Payroll", model.VisibilityPrivate)
	vendors := table(ana, "Vendors", model.VisibilityWorkspace)
	leads := table(ben, "Leads", model.VisibilityPrivate)
	name, err := business.CreateField(ctx, vendors.Id, business.FieldInput{Name: "Vendor", Type: model.FieldText}, business.Actor{UserID: ana.Id})
	if err != nil {
		t.Fatalf("create field: %v", err)
	}

	// recorded is the log's rows of action, once what was handed to its
	// writer is written.
	recorded := func(action string) []exportRecord {
		t.Helper()
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("the audit log's writer is still busy: %v", busy)
		}
		rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `SELECT coalesce(actor_id::text, ''), coalesce(actor_email, ''), category,
			coalesce(metadata::text, '{}') FROM admin_audit_log WHERE action = $1 ORDER BY seq`, action)
		if err != nil {
			t.Fatalf("read the audit log: %v", err)
		}
		defer rows.Close()
		var out []exportRecord
		for rows.Next() {
			var r exportRecord
			var meta string
			if err := rows.Scan(&r.Actor, &r.Email, &r.Category, &meta); err != nil {
				t.Fatal(err)
			}
			var m struct {
				Tables []string `json:"table_ids"`
			}
			if err := json.Unmarshal([]byte(meta), &m); err != nil {
				t.Fatalf("an export's metadata %s: %v", meta, err)
			}
			sort.Strings(m.Tables)
			r.Tables = m.Tables
			out = append(out, r)
		}
		return out
	}
	is := func(t *testing.T, what string, got []exportRecord, want ...exportRecord) {
		t.Helper()
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if string(g) != string(w) {
			t.Errorf("%s: the audit log holds %s; want %s", what, g, w)
		}
	}
	ids := func(tables ...*model.DataTable) []string {
		out := make([]string, len(tables))
		for i, tb := range tables {
			out[i] = tb.Id.String()
		}
		sort.Strings(out)
		return out
	}
	const workbook = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	t.Run("every table he can open: one row naming him and exactly its tables", func(t *testing.T) {
		w := exportAs(ben, ExportTables, uuid.Nil, "")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != workbook {
			t.Fatalf("Ben's export of every table answered %d %q %.200s; want a workbook", w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		is(t, "Ben's export of every table", recorded("tables.exported"),
			exportRecord{Actor: ben.Id.String(), Email: ben.EmailID, Category: "tables", Tables: ids(vendors, leads)})
	})

	t.Run("one table: one row naming him and that table", func(t *testing.T) {
		w := exportAs(ben, ExportTable, vendors.Id, `{"fields":["`+name.Id.String()+`"]}`)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != workbook {
			t.Fatalf("Ben's export of Vendors answered %d %q %.200s; want a workbook", w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		is(t, "Ben's export of Vendors", recorded("table.exported"),
			exportRecord{Actor: ben.Id.String(), Email: ben.EmailID, Category: "tables", Tables: ids(vendors)})
	})

	t.Run("an export refused writes no row", func(t *testing.T) {
		if w := exportAs(ben, ExportTable, payroll.Id, `{"fields":[]}`); w.Code != http.StatusNotFound {
			t.Errorf("Ben's export of Ana's private Payroll answered %d %.200s; want not found", w.Code, w.Body)
		}
		if got := recorded("table.exported"); len(got) != 1 {
			t.Errorf("after a refused export of one table the log holds %d rows of it; want 1, Vendors' alone", len(got))
		}
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM admin_audit_log WHERE metadata::text LIKE '%' || $1 || '%'`,
			payroll.Id.String()).Scan(&n); err != nil || n != 0 {
			t.Errorf("rows of the audit log naming Ana's private Payroll: %d (%v); want none, Ben exported nothing of it", n, err)
		}
	})

	t.Run("an export of every table refused writes no row", func(t *testing.T) {
		// Ana closes Vendors to the workspace for a moment: Cy, new to it,
		// can open no table, so his export of every table is refused (422).
		cy := seedMember(t, ctx, "cy")
		visibility := func(v string) {
			t.Helper()
			if _, _, err := business.UpdateTable(ctx, vendors.Id, business.TablePatch{Visibility: &v}, business.Actor{UserID: ana.Id}); err != nil {
				t.Fatalf("make Vendors %s: %v", v, err)
			}
		}
		visibility(model.VisibilityPrivate)
		t.Cleanup(func() { visibility(model.VisibilityWorkspace) })
		if w := exportAs(cy, ExportTables, uuid.Nil, ""); w.Code != http.StatusUnprocessableEntity || w.Header().Get("Content-Type") == workbook {
			t.Fatalf("Cy's export of every table, who can open none, answered %d %q %.200s; want refused in words (422)", w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		is(t, "after Cy's refused export of every table", recorded("tables.exported"),
			exportRecord{Actor: ben.Id.String(), Email: ben.EmailID, Category: "tables", Tables: ids(vendors, leads)})
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM admin_audit_log WHERE actor_id = $1`, cy.Id).Scan(&n); err != nil || n != 0 {
			t.Errorf("rows of the audit log by Cy: %d (%v); want none, he exported nothing", n, err)
		}
	})
}

// logLines is a log that keeps each line written to it, with its level.
type logLines struct {
	mu   sync.Mutex
	said []slog.Record
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.said = append(l.said, r)
	return nil
}
func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }

func TestAnExportGoesAheadWhenItsAuditRowCantBeWrittenAndTheLogSaysSo(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ben := seedMember(t, ctx, "ben")
	actor := business.Actor{UserID: ben.Id}
	leads, err := business.CreateTable(ctx, business.TableInput{Name: "Leads", Visibility: model.VisibilityPrivate}, actor)
	if err != nil {
		t.Fatalf("create Leads: %v", err)
	}
	company, err := business.CreateField(ctx, leads.Id, business.FieldInput{Name: "Company", Type: model.FieldText}, actor)
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	if _, err := business.CreateRow(ctx, leads.Id, business.RowInput{Values: map[string]interface{}{company.Id.String(): "Zephyr Freight"}}, actor); err != nil {
		t.Fatalf("create row: %v", err)
	}
	for _, s := range []string{
		`CREATE FUNCTION refuse_export_rows() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.action IN ('tables.exported', 'table.exported') THEN
				RAISE EXCEPTION 'the audit log refuses this row';
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER refuse_export_rows BEFORE INSERT ON admin_audit_log FOR EACH ROW EXECUTE FUNCTION refuse_export_rows()`,
	} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	log := &logLines{}
	was := helpers.Logger
	helpers.Logger = slog.New(log)
	t.Cleanup(func() { helpers.Logger = was })

	for _, c := range []struct {
		action  string
		handler http.HandlerFunc
		table   uuid.UUID
		body    string
	}{
		{"tables.exported", ExportTables, uuid.Nil, ""},
		{"table.exported", ExportTable, leads.Id, `{"fields":["` + company.Id.String() + `"]}`},
	} {
		w := exportAs(ben, c.handler, c.table, c.body)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s with its audit row refused answered %d (%d bytes); want the workbook all the same", c.action, w.Code, w.Body.Len())
		}
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("the audit log's writer is still busy: %v", busy)
		}
		log.mu.Lock()
		var errs []string
		for _, r := range log.said {
			if r.Level == slog.LevelError && strings.Contains(r.Message, c.action) {
				errs = append(errs, r.Message)
			}
			for _, data := range []string{"Leads", "Company", "Zephyr Freight"} {
				if strings.Contains(r.Message, data) {
					t.Errorf("%s: a log line holds the table's data %q: %s", c.action, data, r.Message)
				}
			}
		}
		log.said = nil
		log.mu.Unlock()
		if len(errs) != 1 {
			t.Errorf("%s with its audit row refused: error lines naming it %q; want one, so an admin sees the gap", c.action, errs)
		}
	}
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM admin_audit_log WHERE action LIKE '%exported'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("export rows in the log: %d (%v); want none, each was refused", n, err)
	}
}
