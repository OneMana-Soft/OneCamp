//go:build integration

package business

// A table's own forms (tables B5), end to end against Postgres 12 and
// Dgraph, through the tables' own writer.
// Run: PKGS=./business/Form/ integ_rel.sh <wt> <log> '^(TestATableKeepsItsOwnForms)$'

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/business/Formula"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The table's maker makes a form from the table, with no project: it is the
// table's alone (no project lists it), a visitor's answer adds a row to the
// table as its owner, and a member who can add rows but not manage the table
// can't list, make or delete its forms. Deleted, its link stops and the row
// it added stays.
func TestATableKeepsItsOwnForms(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)
	maker, member := uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, u := range []uuid.UUID{maker, member} {
		exec(`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`, u, u.String()+"@example.test", "u-"+u.String()[:8], "Someone")
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:maker", "user_uuid": maker.String(), "user_name": "Maker", "dgraph.type": "User"},
		{"uid": "_:member", "user_uuid": member.String(), "user_name": "Member", "dgraph.type": "User"},
	})
	by := dataTableBusiness.Actor{UserID: maker}
	table, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Leads", Visibility: tableModel.VisibilityWorkspace}, by)
	if err != nil {
		t.Fatalf("make the table: %v", err)
	}
	company, err := dataTableBusiness.CreateField(ctx, table.Id, dataTableBusiness.FieldInput{Name: "Company", Type: tableModel.FieldText, Required: true, Position: 1}, by)
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Title: "Lead sign-up", Active: true,
		Fields: []Field{{Id: "company", Label: "Company", Type: ShortText, Required: true, MapsTo: MapColumn + company.Id.String()}}}

	// A member who can add the table's rows, but not manage it.
	if _, err := SaveTableForm(ctx, table.Id, in, member); !errors.Is(err, ErrTableForbidden) {
		t.Errorf("a member's form: %v, want ErrTableForbidden", err)
	}
	f, err := SaveTableForm(ctx, table.Id, in, maker)
	if err != nil {
		t.Fatalf("the maker's form: %v", err)
	}
	if f.ProjectUUID != uuid.Nil || f.TargetTable == nil || *f.TargetTable != table.Id || len(f.Token) != 24 {
		t.Fatalf("saved %+v: want no project, the table, a link", f)
	}
	var home *uuid.UUID
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT project_uuid FROM project_forms WHERE id = $1`, f.Id).Scan(&home); err != nil || home != nil {
		t.Errorf("stored project %v (%v), want NULL", home, err)
	}
	if forms, err := TableForms(ctx, table.Id, maker); err != nil || len(forms) != 1 || forms[0].Id != f.Id {
		t.Errorf("the table's forms: %+v %v", forms, err)
	}
	if forms, err := formModel.List(ctx, uuid.Nil); err != nil || len(forms) != 0 {
		t.Errorf("a project's list holds %d forms (%v), want none", len(forms), err)
	}
	if _, err := TableForms(ctx, table.Id, member); !errors.Is(err, ErrTableForbidden) {
		t.Errorf("a member lists them: %v, want ErrTableForbidden", err)
	}

	if _, err := GetPublic(ctx, f.Token); err != nil {
		t.Fatalf("the form by its link: %v", err)
	}
	// B5b: a page to go to once it's sent, and a question only its link fills, kept by the store.
	in.Id, in.Redirect = f.Id.String(), "https://example.com/thanks"
	in.Fields = append(in.Fields, Field{Id: "src", Label: "Source", Type: ShortText, Hidden: true, MapsTo: MapColumn + company.Id.String()})
	if _, err := SaveTableForm(ctx, table.Id, in, maker); err == nil {
		t.Error("two questions filling one column were saved")
	}
	in.Fields = in.Fields[:1]
	if _, err := SaveTableForm(ctx, table.Id, in, maker); err != nil {
		t.Fatalf("save with a redirect: %v", err)
	}
	if p, err := GetPublic(ctx, f.Token); err != nil || p.Redirect != "https://example.com/thanks" {
		t.Errorf("the form by its link: %+v %v, want its redirect", p, err)
	}
	if err := Submit(ctx, f.Token, map[string]any{"company": "Acme"}, formula.NumberSigns{}, "", "", time.Now()); err != nil {
		t.Fatalf("send it: %v", err)
	}
	rows, err := dataTableBusiness.ListRows(ctx, table.Id, by, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	var cells map[string]any
	_ = json.Unmarshal([]byte(rows[0].Values), &cells)
	if cells[company.Id.String()] != "Acme" {
		t.Errorf("the row's cells: %v", cells)
	}
	if subs, err := formModel.Latest(ctx, f.Id, 5); err != nil || len(subs) != 1 || subs[0].RowID == nil || *subs[0].RowID != rows[0].Id {
		t.Errorf("the submission: %+v %v", subs, err)
	}

	if _, err := DeleteTableForm(ctx, table.Id, f.Id, member); !errors.Is(err, ErrTableForbidden) {
		t.Errorf("a member deletes it: %v, want ErrTableForbidden", err)
	}
	if ok, err := DeleteTableForm(ctx, table.Id, f.Id, maker); err != nil || !ok {
		t.Fatalf("the maker deletes it: %v %v", ok, err)
	}
	if _, err := GetPublic(ctx, f.Token); !errors.Is(err, ErrNoForm) {
		t.Errorf("its link after it was deleted: %v, want ErrNoForm", err)
	}
	if rows, _ := dataTableBusiness.ListRows(ctx, table.Id, by, 10, 0); len(rows) != 1 {
		t.Errorf("%d rows after the form went, want its 1", len(rows))
	}
	// A form with no home at all is refused by the store.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO project_forms (token, title, fields, created_by) VALUES ('x', 'x', '[]', $1)`, maker); err == nil {
		t.Error("a form with neither a project nor a table was stored")
	}
}
