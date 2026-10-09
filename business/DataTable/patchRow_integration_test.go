//go:build integration

package business

// Changing some of a row's cells, against Postgres 12 with every migration:
// the rest of the row and its place stay as they are, and an AI fill holding
// an old copy of the row doesn't undo what changed meanwhile.
// Run: go test -tags=integration ./business/DataTable/ -run TestChangingSomeCells -v

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestChangingSomeCellsLeavesTheRest(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	owner := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	actor := Actor{UserID: owner, IsAdmin: true}
	table, err := CreateTable(ctx, TableInput{Name: "Accounts", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	field := func(name string) string {
		t.Helper()
		f, err := CreateField(ctx, table.Id, FieldInput{Name: name, Type: model.FieldText}, actor)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return f.Id.String()
	}
	name, stage, notes := field("Name"), field("Stage"), field("Notes")
	row, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{name: "Acme", stage: "Lead"}, Position: 5}, actor)
	if err != nil {
		t.Fatalf("create row: %v", err)
	}
	stored := func() (map[string]interface{}, float64) {
		t.Helper()
		r, err := model.GetRowByID(ctx, table.Id, row.Id)
		if err != nil || r == nil {
			t.Fatalf("read row: %v", err)
		}
		return parseRowValues(r.Values), r.Position
	}

	// An agent sets one cell: the others and the row's place stay.
	if _, err := PatchRow(ctx, table.Id, row.Id, map[string]interface{}{stage: "Won"}, actor); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if v, pos := stored(); v[name] != "Acme" || v[stage] != "Won" || pos != 5 {
		t.Errorf("after setting one cell: %v at %v", v, pos)
	}

	// An AI fill read the row; then someone changed a cell and moved the row;
	// then the fill wrote its answer. Both changes stand.
	stale, err := model.GetRowByID(ctx, table.Id, row.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateRow(ctx, table.Id, row.Id, RowInput{Values: map[string]interface{}{name: "Acme Ltd", stage: "Won"}, Position: 9}, actor); err != nil {
		t.Fatalf("a person's edit: %v", err)
	}
	if err := writeCell(ctx, table, stale, notes, "Big account"); err != nil {
		t.Fatalf("the fill: %v", err)
	}
	if v, pos := stored(); v[name] != "Acme Ltd" || v[stage] != "Won" || v[notes] != "Big account" || pos != 9 {
		t.Errorf("after the fill: %v at %v", v, pos)
	}

	// A change that would make the row too big is refused, and changes nothing.
	if _, err := PatchRow(ctx, table.Id, row.Id, map[string]interface{}{notes: strings.Repeat("x", maxValuesBytes)}, actor); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Errorf("an oversized cell: %v", err)
	}
	if v, _ := stored(); v[notes] != "Big account" {
		t.Errorf("an oversized cell changed the row: %v", v[notes])
	}

	// A deleted row isn't brought back by a change.
	if err := DeleteRow(ctx, table.Id, row.Id, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchRow(ctx, table.Id, row.Id, map[string]interface{}{stage: "Lost"}, actor); err == nil {
		t.Error("a deleted row was changed")
	}
}
