//go:build integration

package controllers

// An export's workbook is written as it streams out (sendWorkbook), so a
// download that breaks off part way can't be answered in words: the
// connection ends unfinished, and one error line says so, never the tables'
// data (no line at any level holds a table's name, a field's name or a
// cell's value). Against Postgres 12 with every migration: Ben's private
// Payroll, its field and its one row, exported alone and with every table he
// can open, each through a connection whose every write fails.
// An export reads its first rows before the file begins, so rows that can't
// be read are answered in words (a 503 to try again), with no workbook byte
// and no row in the admin audit log; never as a file begun and broken off.
// Run: logs/tables-198/expall/b219/integ219.sh with PK='./controllers/DataTable/' and the tests' names.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// brokenDownload is a reader's connection that breaks as the file begins:
// every write to it fails.
type brokenDownload struct {
	header http.Header
	tried  bool
}

func (b *brokenDownload) Header() http.Header { return b.header }
func (b *brokenDownload) WriteHeader(int)     {}
func (b *brokenDownload) Write([]byte) (int, error) {
	b.tried = true
	return 0, errors.New("write tcp 10.0.0.2:443->10.0.0.9:51544: write: broken pipe")
}

func TestADownloadThatBreaksOffLogsOneErrorLineWithoutTheTablesData(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ben := seedMember(t, ctx, "ben")
	actor := business.Actor{UserID: ben.Id}
	payroll, err := business.CreateTable(ctx, business.TableInput{Name: "Payroll", Visibility: model.VisibilityPrivate}, actor)
	if err != nil {
		t.Fatalf("create Payroll: %v", err)
	}
	band, err := business.CreateField(ctx, payroll.Id, business.FieldInput{Name: "Salary band", Type: model.FieldText}, actor)
	if err != nil {
		t.Fatalf("create field: %v", err)
	}
	if _, err := business.CreateRow(ctx, payroll.Id, business.RowInput{Values: map[string]interface{}{band.Id.String(): "Zephyr Freight"}}, actor); err != nil {
		t.Fatalf("create row: %v", err)
	}
	// The seed's own work in the background is done before the log is read.
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("the seed's background work is still busy: %v", busy)
	}
	log := &logLines{}
	was := helpers.Logger
	helpers.Logger = slog.New(log)
	t.Cleanup(func() { helpers.Logger = was })

	for _, c := range []struct {
		what    string
		handler http.HandlerFunc
		table   uuid.UUID
		body    string
	}{
		{"Payroll's export", ExportTable, payroll.Id, `{"fields":["` + band.Id.String() + `"]}`},
		{"the export of every table", ExportTables, uuid.Nil, ""},
	} {
		w := &brokenDownload{header: http.Header{}}
		func() {
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Errorf("%s through a broken connection ended with %v; want the handler aborted (http.ErrAbortHandler), the file left unfinished", c.what, got)
				}
			}()
			exportTo(w, ben, c.handler, c.table, c.body)
		}()
		if !w.tried {
			t.Errorf("%s never began the file; want it broken off part way", c.what)
		}
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("the audit log's writer is still busy: %v", busy)
		}
		log.mu.Lock()
		errs := 0
		for _, r := range log.said {
			if r.Level == slog.LevelError && strings.HasPrefix(r.Message, "controllers/DataTable ") {
				errs++
			}
			line := r.Message
			r.Attrs(func(a slog.Attr) bool {
				line += " " + a.Value.String()
				return true
			})
			for _, data := range []string{"Payroll", "Salary band", "Zephyr Freight"} {
				if strings.Contains(line, data) {
					t.Errorf("%s: a log line holds the table's data %q: %s", c.what, data, line)
				}
			}
		}
		log.said = nil
		log.mu.Unlock()
		if errs != 1 {
			t.Errorf("%s through a broken connection: %d error lines; want one from the export's handler, so an admin sees the download broke off", c.what, errs)
		}
	}
}

// tableRowsDown is where every spec is run, with a table's rows unreadable:
// the store's failure, as a reader meets it.
type tableRowsDown struct{ query.Registry }

func (d tableRowsDown) Run(ctx context.Context, who accessBusiness.Subject, spec query.Spec, page query.Page) (query.Result, error) {
	if spec.Scope.Kind == query.ScopeTable {
		return query.Result{}, errors.New("read rows: the store is down")
	}
	return d.Registry.Run(ctx, who, spec, page)
}

func TestAnExportWhoseFirstRowsCantBeReadIsAnsweredBeforeTheFileBegins(t *testing.T) {
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
	was := query.Queries
	query.Queries = tableRowsDown{was}
	t.Cleanup(func() { query.Queries = was })
	const workbook = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	for _, c := range []struct {
		what, action string
		handler      http.HandlerFunc
		table        uuid.UUID
		body         string
	}{
		{"the export of every table", "tables.exported", ExportTables, uuid.Nil, ""},
		{"Leads' export", "table.exported", ExportTable, leads.Id, `{"fields":["` + company.Id.String() + `"]}`},
	} {
		w := httptest.NewRecorder()
		func() {
			defer func() {
				if got := recover(); got != nil {
					t.Errorf("%s, its first rows unreadable, began the file and broke it off (%v); want an answer in words before any of it", c.what, got)
				}
			}()
			exportTo(w, ben, c.handler, c.table, c.body)
		}()
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Content-Type") == workbook || bytes.HasPrefix(w.Body.Bytes(), []byte("PK")) {
			t.Errorf("%s, its first rows unreadable, answered %d %q %.80q; want a 503 to try again, and no workbook byte", c.what, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("the audit log's writer is still busy: %v", busy)
		}
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM admin_audit_log WHERE action = $1`, c.action).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s, never sent: rows of it in the audit log %d (%v); want none", c.what, n, err)
		}
	}
}
