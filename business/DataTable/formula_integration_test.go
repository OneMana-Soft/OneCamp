//go:build integration

package business

// Formula fields end to end, against Postgres 12 with every migration: the
// field types are allowed (migration 196, relation included), a formula is
// checked and stored with fields by id, rows come back with their values
// worked out and never store them, a renamed field keeps working, a deleted
// one says so, and totals can filter on a formula.
// Run: go test -tags=integration ./business/DataTable/ -run 'TestFormula' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestFormulaFields(t *testing.T) {
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
	table, err := CreateTable(ctx, TableInput{Name: "Launch budget", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	field := func(name, typ string, cfg map[string]interface{}) *model.Field {
		t.Helper()
		f, err := CreateField(ctx, table.Id, FieldInput{Name: name, Type: typ, Config: cfg}, actor)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return f
	}
	price := field("Price", model.FieldNumber, nil)
	qty := field("Quantity", model.FieldNumber, nil)
	due := field("Due", model.FieldDate, nil)
	done := field("Done", model.FieldCheckbox, nil)
	field("Linked work", model.FieldRelation, map[string]interface{}{"relation_target": "task"})
	total := field("Total", model.FieldFormula, map[string]interface{}{"formula": "{Price} * {quantity}"})
	late := field("Late", model.FieldFormula, map[string]interface{}{"formula": `IF(AND(NOT({Done}), {Due} < TODAY()), "Late", "")`})

	// Stored with fields by id.
	if want := `{"formula": "{#` + price.Id.String() + `} * {#` + qty.Id.String() + `}"}`; !jsonEqual(total.Config, want) {
		t.Errorf("total stored as %s", total.Config)
	}
	// A formula that can't be read isn't saved, and says why.
	if _, err := CreateField(ctx, table.Id, FieldInput{Name: "Bad", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "{Cost} * 2"}}, actor); err == nil ||
		!strings.Contains(err.Error(), `There's no field called "Cost"`) {
		t.Errorf("a formula reading no field: %v", err)
	}

	row, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{
		price.Id.String(): 12.5,
		qty.Id.String():   4,
		due.Id.String():   "2020-01-01",
		done.Id.String():  false,
		total.Id.String(): 999, // sent back by the grid; never stored
	}}, actor)
	if err != nil {
		t.Fatalf("create row: %v", err)
	}
	if v := parseRowValues(row.Values); v[total.Id.String()] != float64(50) || v[late.Id.String()] != "Late" {
		t.Errorf("the new row came back with %s", row.Values)
	}
	stored, err := model.GetRowByID(ctx, table.Id, row.Id)
	if err != nil || stored == nil {
		t.Fatalf("read row: %v", err)
	}
	if v := parseRowValues(stored.Values); v[total.Id.String()] != nil || v[late.Id.String()] != nil {
		t.Errorf("formula values were stored: %s", stored.Values)
	}

	// Read back: values worked out, the formula by name, and what it gives.
	bundle, err := GetBundle(ctx, table.Id, actor)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if cfg := configOf(bundle, total.Id); cfg["formula"] != "{Price} * {Quantity}" || cfg["result"] != "number" {
		t.Errorf("total reads %v", cfg)
	}
	if cfg := configOf(bundle, late.Id); cfg["result"] != "text" {
		t.Errorf("late reads %v", cfg)
	}
	if v := parseRowValues(bundle.Rows[0].Values); v[total.Id.String()] != float64(50) {
		t.Errorf("the bundle's row: %s", bundle.Rows[0].Values)
	}

	// Totals can filter on a formula: it isn't in the stored values, so it
	// mustn't be pushed down to the database's text match.
	agg, _, err := AggregateTable(ctx, table.Id, actor, QuerySpec{Op: "count", Filters: []Filter{{Field: "Late", Op: "eq", Value: "Late"}}})
	if err != nil || agg == nil || agg.MatchedRows != 1 {
		t.Errorf("count where Late is Late: %+v, %v", agg, err)
	}

	// Renamed: shown under the new name, and still worked out.
	if err := UpdateField(ctx, table.Id, price.Id, FieldInput{Name: "Unit price", Type: model.FieldNumber}, actor); err != nil {
		t.Fatalf("rename: %v", err)
	}
	bundle, _ = GetBundle(ctx, table.Id, actor)
	if cfg := configOf(bundle, total.Id); cfg["formula"] != "{Unit price} * {Quantity}" {
		t.Errorf("after the rename: %v", cfg)
	}
	if v := parseRowValues(bundle.Rows[0].Values); v[total.Id.String()] != float64(50) {
		t.Errorf("after the rename the row has %s", bundle.Rows[0].Values)
	}

	// The preview works out a draft on the first rows.
	preview, err := PreviewFormula(ctx, table.Id, uuid.Nil, "{Unit price} + 1", actor)
	if err != nil || preview.Result != "number" || preview.Error != "" || len(preview.Values) != 1 || preview.Values[0] != 13.5 {
		t.Errorf("preview: %+v, %v", preview, err)
	}
	preview, _ = PreviewFormula(ctx, table.Id, total.Id, "{Total} + 1", actor)
	if preview == nil || preview.Error != "A formula can't read its own value" {
		t.Errorf("a formula reading itself: %+v", preview)
	}

	// Deleted: every row says why.
	if err := DeleteField(ctx, table.Id, qty.Id, actor); err != nil {
		t.Fatalf("delete: %v", err)
	}
	bundle, _ = GetBundle(ctx, table.Id, actor)
	if cfg := configOf(bundle, total.Id); cfg["error"] != "A field this formula reads has been deleted" || cfg["formula"] != "{Unit price} * {Deleted field}" {
		t.Errorf("after the delete: %v", cfg)
	}
	if m, ok := parseRowValues(bundle.Rows[0].Values)[total.Id.String()].(map[string]interface{}); !ok || m["error"] == nil {
		t.Errorf("after the delete the row has %s", bundle.Rows[0].Values)
	}
}

func configOf(b *TableBundle, id uuid.UUID) map[string]interface{} {
	for _, f := range b.Fields {
		if f.Id == id {
			out := map[string]interface{}{}
			_ = json.Unmarshal([]byte(f.Config), &out)
			return out
		}
	}
	return nil
}

func jsonEqual(a, b string) bool {
	var x, y interface{}
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

// A table made from a template (the demo's launch budget, a template from the
// gallery) gets its formulas after the fields they read, wherever they're
// listed, and stores them by id like one added by hand.
func TestFormulaFieldsFromATemplate(t *testing.T) {
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
	actor := Actor{UserID: owner}
	table, err := CreateTableFromTemplate(ctx, TableInput{Name: "Budget", Visibility: "workspace"}, []FieldInput{
		// Listed before what they read: a formula that reads a formula, too.
		{Name: "With tax", Type: model.FieldFormula, Position: 3, Config: map[string]interface{}{"formula": "{Total} * 1.18"}},
		{Name: "Total", Type: model.FieldFormula, Position: 2, Config: map[string]interface{}{"formula": "{Cost} * {Quantity}"}},
		{Name: "Cost", Type: model.FieldNumber, Position: 0},
		{Name: "Quantity", Type: model.FieldNumber, Position: 1},
		// Can't be read: left out, and the table is still made.
		{Name: "Broken", Type: model.FieldFormula, Position: 3, Config: map[string]interface{}{"formula": "{Nope}"}},
		{Name: "Reads broken", Type: model.FieldFormula, Position: 4, Config: map[string]interface{}{"formula": "{Broken} & 1"}},
		{Name: "Loop A", Type: model.FieldFormula, Position: 5, Config: map[string]interface{}{"formula": "{Loop B} + 1"}},
		{Name: "Loop B", Type: model.FieldFormula, Position: 6, Config: map[string]interface{}{"formula": "{Loop A} + 1"}},
		// A chain listed last first.
		{Name: "Step 4", Type: model.FieldFormula, Position: 7, Config: map[string]interface{}{"formula": "{Step 3} + 1"}},
		{Name: "Step 3", Type: model.FieldFormula, Position: 8, Config: map[string]interface{}{"formula": "{Step 2} + 1"}},
		{Name: "Step 2", Type: model.FieldFormula, Position: 9, Config: map[string]interface{}{"formula": "{Step 1} + 1"}},
		{Name: "Step 1", Type: model.FieldFormula, Position: 10, Config: map[string]interface{}{"formula": "{With tax} + 1"}},
	}, nil, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fields, err := model.ListFields(ctx, table.Id)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	byName := map[string]*model.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"Broken", "Reads broken", "Loop A", "Loop B"} {
		if byName[name] != nil {
			t.Errorf("%s, which can't be worked out, was made", name)
		}
	}
	for _, name := range []string{"Step 1", "Step 2", "Step 3", "Step 4"} {
		if byName[name] == nil {
			t.Errorf("%s, in a chain listed last first, wasn't made", name)
		}
	}
	if byName["With tax"] == nil {
		t.Error("a formula reading a formula listed after it was dropped")
	}
	total, cost, qty := byName["Total"], byName["Cost"], byName["Quantity"]
	if total == nil || cost == nil || qty == nil {
		t.Fatalf("fields made: %v", byName)
	}
	if want := `{"formula": "{#` + cost.Id.String() + `} * {#` + qty.Id.String() + `}"}`; !jsonEqual(total.Config, want) {
		t.Errorf("total stored as %s", total.Config)
	}
	if _, err := CreateRow(ctx, table.Id, RowInput{Values: map[string]interface{}{cost.Id.String(): 6.5, qty.Id.String(): 120}}, actor); err != nil {
		t.Fatalf("row: %v", err)
	}
	bundle, err := GetBundle(ctx, table.Id, actor)
	if err != nil || len(bundle.Rows) != 1 {
		t.Fatalf("bundle: %v", err)
	}
	if v := parseRowValues(bundle.Rows[0].Values); v[total.Id.String()] != float64(780) || v[byName["Step 4"].Id.String()] != 780*1.18+4 {
		t.Errorf("the row: %s", bundle.Rows[0].Values)
	}

	// Two formulas with one name: the first reads a formula made after the
	// second, and is still made.
	twins, err := CreateTableFromTemplate(ctx, TableInput{Name: "Twins", Visibility: "workspace"}, []FieldInput{
		{Name: "X", Type: model.FieldFormula, Position: 0, Config: map[string]interface{}{"formula": "{Y} + 1"}},
		{Name: "X", Type: model.FieldFormula, Position: 1, Config: map[string]interface{}{"formula": "5"}},
		{Name: "Y", Type: model.FieldFormula, Position: 2, Config: map[string]interface{}{"formula": "{X} * 2"}},
	}, nil, actor)
	if err != nil {
		t.Fatalf("twins: %v", err)
	}
	if made, _ := model.ListFields(ctx, twins.Id); len(made) != 3 {
		t.Errorf("twins: %d of 3 formulas made", len(made))
	}
}

// A table has room for 100 formula fields; the 101st is refused, and its
// preview says why, while those it has can still be changed.
func TestFormulaFieldsHaveALimit(t *testing.T) {
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
	actor := Actor{UserID: owner}
	table, err := CreateTable(ctx, TableInput{Name: "Many", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var first *model.Field
	for i := 0; i < maxFormulaFields; i++ {
		f, err := CreateField(ctx, table.Id, FieldInput{Name: fmt.Sprint("F", i), Type: model.FieldFormula, Config: map[string]interface{}{"formula": fmt.Sprint(i, " * 2")}}, actor)
		if err != nil {
			t.Fatalf("formula %d: %v", i, err)
		}
		if first == nil {
			first = f
		}
	}
	want := "A table can have at most 100 formula fields"
	if _, err := CreateField(ctx, table.Id, FieldInput{Name: "One more", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "1"}}, actor); err == nil || err.Error() != want {
		t.Errorf("the 101st: %v", err)
	}
	if p, err := PreviewFormula(ctx, table.Id, uuid.Nil, "1", actor); err != nil || p.Error != want {
		t.Errorf("its preview: %+v, %v", p, err)
	}
	if err := UpdateField(ctx, table.Id, first.Id, FieldInput{Name: "F0", Type: model.FieldFormula, Config: map[string]interface{}{"formula": "3"}}, actor); err != nil {
		t.Errorf("changing one it has: %v", err)
	}
}
