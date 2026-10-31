//go:build integration

package business

// A form that adds table rows, end to end against Postgres 12 and Dgraph,
// through the tables' own writer.
// Run: PKGS=./business/Form/ integ_rel.sh <wt> <log> '^(TestAFormAddsARowToItsTable)$'

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

// A vendor sign-up form adds a row to the Vendors table as its owner: a
// column every row needs must be asked of everyone, a choice offers the
// column's options, the row's cells are the answers by kind, and the
// submission keeps the row. A table its saver can't add rows to isn't one a
// form can fill, and a form whose owner loses the table closes.
func TestAFormAddsARowToItsTable(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)
	owner, creator, project := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, u := range []uuid.UUID{owner, creator} {
		exec(`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`, u, u.String()+"@example.test", "u-"+u.String()[:8], "Someone")
	}
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, owner)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "user_uuid": owner.String(), "user_name": "Owner", "dgraph.type": "User"},
		{"uid": "_:team", "team_uuid": uuid.NewString(), "team_name": "Team", "dgraph.type": "Team"},
		{"uid": "_:project", "project_uuid": project.String(), "project_name": "Launch", "dgraph.type": "Project",
			"project_team": map[string]any{"uid": "_:team"}, "project_members": []map[string]any{{"uid": "_:owner"}}},
	})

	// The Vendors table is someone else's, shown to the workspace.
	by := dataTableBusiness.Actor{UserID: creator}
	table, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Vendors", Visibility: tableModel.VisibilityWorkspace}, by)
	if err != nil {
		t.Fatalf("make the table: %v", err)
	}
	field := func(in dataTableBusiness.FieldInput) *tableModel.Field {
		t.Helper()
		f, err := dataTableBusiness.CreateField(ctx, table.Id, in, by)
		if err != nil {
			t.Fatalf("add %s: %v", in.Name, err)
		}
		return f
	}
	company := field(dataTableBusiness.FieldInput{Name: "Company", Type: tableModel.FieldText, Required: true, Position: 1})
	email := field(dataTableBusiness.FieldInput{Name: "Email", Type: tableModel.FieldEmail, Required: true, Position: 2})
	budget := field(dataTableBusiness.FieldInput{Name: "Budget", Type: tableModel.FieldNumber, Position: 3})
	tier := field(dataTableBusiness.FieldInput{Name: "Tier", Type: tableModel.FieldSelect, Position: 4,
		Config: map[string]interface{}{"options": []interface{}{map[string]interface{}{"label": "Gold"}, map[string]interface{}{"label": "Silver"}}}})

	questions := func(emailRequired bool) []Field {
		return []Field{
			{Id: "company", Label: "Company", Type: ShortText, Required: true, MapsTo: MapColumn + company.Id.String()},
			{Id: "email", Label: "Work email", Type: Email, Required: emailRequired, MapsTo: MapColumn + email.Id.String()},
			{Id: "budget", Label: "Budget", Type: Number, MapsTo: MapColumn + budget.Id.String()},
			{Id: "tier", Label: "Tier", Type: Select, Options: []string{"a", "b"}, MapsTo: MapColumn + tier.Id.String()},
		}
	}
	in := func(q []Field) Input {
		return Input{Title: "Vendor sign-up", Active: true, TargetTable: table.Id.String(), Fields: q}
	}
	var fe *FormError
	if _, err := Save(ctx, project, in(questions(false)), owner); !errors.As(err, &fe) {
		t.Errorf("a form leaving out a column every row needs: %v, want a FormError", err)
	}
	f, err := Save(ctx, project, in(questions(true)), owner)
	if err != nil {
		t.Fatalf("save the sign-up form: %v", err)
	}
	var stored []Field
	_ = json.Unmarshal(f.Fields, &stored)
	if len(stored[3].Options) != 2 || stored[3].Options[0] != "Gold" {
		t.Errorf("the tier question offers %v, not the column's options", stored[3].Options)
	}

	answers := map[string]any{"company": "Acme", "email": "ops@acme.test", "budget": "1,200", "tier": "Gold"}
	if err := Submit(ctx, f.Token, answers, formula.NumberSigns{}, "", "", time.Now()); err != nil {
		t.Fatalf("send the form: %v", err)
	}
	rows, err := dataTableBusiness.ListRows(ctx, table.Id, by, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	var cells map[string]any
	if err := json.Unmarshal([]byte(rows[0].Values), &cells); err != nil {
		t.Fatal(err)
	}
	if cells[company.Id.String()] != "Acme" || cells[email.Id.String()] != "ops@acme.test" || cells[budget.Id.String()] != 1200.0 || cells[tier.Id.String()] != "Gold" {
		t.Errorf("the row's cells are %v", cells)
	}
	subs, err := formModel.Latest(ctx, f.Id, 5)
	if err != nil || len(subs) != 1 || subs[0].RowID == nil || *subs[0].RowID != rows[0].Id || subs[0].TaskUUID != nil {
		t.Fatalf("the submission: %+v %v", subs, err)
	}

	// Someone's private table is no form's to fill.
	mine, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Mine", Visibility: tableModel.VisibilityPrivate}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Save(ctx, project, Input{Title: "x", Active: true, TargetTable: mine.Id.String(),
		Fields: []Field{{Id: "a", Label: "A", Type: ShortText, MapsTo: MapColumn + uuid.NewString()}}}, owner); !errors.As(err, &fe) {
		t.Errorf("a form filling a private table of someone else's: %v, want a FormError", err)
	}

	// The table made private by its maker: the form's owner can't add rows
	// any more, so the form closes.
	private := tableModel.VisibilityPrivate
	if _, _, err := dataTableBusiness.UpdateTable(ctx, table.Id, dataTableBusiness.TablePatch{Visibility: &private}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := GetPublic(ctx, f.Token); !errors.Is(err, ErrNoForm) {
		t.Errorf("the form after its owner lost the table: %v, want ErrNoForm", err)
	}
}
