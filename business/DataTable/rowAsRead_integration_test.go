//go:build integration

package business

// A row just written comes back as a read of it answers (tables A7), against
// Postgres 12 with every migration: its formula worked out with the new value,
// and its comment count, so the grid puts it where it holds the row with no
// fetch. A save used to fetch the whole bundle again: 534 ms at 100,000 rows.
// Run: integ_rel.sh <worktree> <log> '^(TestARowWrittenComesBackAsARead)$' with PKGS='./business/DataTable/'

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestARowWrittenComesBackAsARead(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	integration.SetupDgraph(t)
	owner := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'Owner', NOW(), NOW())`, owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	actor := Actor{UserID: owner, IsAdmin: true}
	table, err := CreateTable(ctx, TableInput{Name: "Orders", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	field := func(name, typ string, cfg map[string]interface{}) *model.Field {
		t.Helper()
		f, err := CreateField(ctx, table.Id, FieldInput{Name: name, Type: typ, Config: cfg}, actor)
		if err != nil {
			t.Fatalf("field %s: %v", name, err)
		}
		return f
	}
	price := field("Price", model.FieldNumber, nil)
	qty := field("Qty", model.FieldNumber, nil)
	total := field("Total", model.FieldFormula, map[string]interface{}{"formula": "{Price} * {Qty}"})
	row, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{price.Id.String(): 10.0, qty.Id.String(): 3.0}}, actor)
	if err != nil {
		t.Fatal(err)
	}

	// The row's open comments, as the threads store would count them.
	counts := rowCommentCounts
	rowCommentCounts = func(context.Context, []string) (map[string]int, error) {
		return map[string]int{row.Id.String(): 2}, nil
	}
	t.Cleanup(func() { rowCommentCounts = counts })

	written, err := PatchRow(ctx, table.Id, row.Id, map[string]interface{}{price.Id.String(): 12.0}, actor)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	read := RowAsRead(ctx, table.Id, actor, written)
	values := parseRowValues(read.Values)
	if values[total.Id.String()] != 36.0 {
		t.Errorf("the total, worked out with the new price: %v", values[total.Id.String()])
	}
	if read.Comments != 2 {
		t.Errorf("its comment count, as a read gives it: %d", read.Comments)
	}

	// A row the reader can no longer see comes back as it was written.
	gone := &model.Row{Id: uuid.New(), Values: `{}`}
	if got := RowAsRead(ctx, table.Id, actor, gone); got != gone {
		t.Errorf("a row the read doesn't find: %+v", got)
	}
}
