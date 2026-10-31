//go:build integration

package business_test

// A table whose cell index was made whole before its entries held sort keys
// (tables T5c) is made whole again by the repair sweep, and until it is, a
// sort on a text or a choice is answered by the evaluator, in the same
// order, while the index answers the rest; its bundle tells the browser
// which.
//
// Run: integ_rel.sh <worktree> <log> '^(TestATableIndexedBeforeSortKeysIsMadeWholeAgain)$' with PKGS='./business/RowQuery/'

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// plan is the plan a window's cursor names: "walk" or "sql" (the index),
// "worked" (the evaluator).
func plan(t *testing.T, c query.Cursor) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	var m struct {
		K string `json:"k"`
	}
	if err != nil || json.Unmarshal(b, &m) != nil {
		t.Fatalf("a cursor that isn't one: %q", c)
	}
	return m.K
}

func TestATableIndexedBeforeSortKeysIsMadeWholeAgain(t *testing.T) {
	ctx, owner := tableOwner(t)
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Items", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	item, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Item", Type: model.FieldText}, owner)
	if err != nil {
		t.Fatal(err)
	}
	count, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Count", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"Row 10", "row 2", "Row 1", "Éclair", "eclair 3", "", "Booth", "BOOTH 007", "booth 7"}
	for i, s := range names {
		vals, _ := json.Marshal(map[string]interface{}{item.Id.String(): s, count.Id.String(): float64(len(names) - i)})
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO data_table_rows (id, table_id, "values", position, position_key, row_no, created_by, created_at, updated_at)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, NOW(), NOW())`, uuid.New(), tb.Id, string(vals), float64(i), fmt.Sprintf("d%04d", i), i+1, owner.UserID); err != nil {
			t.Fatal(err)
		}
	}
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	// As a table indexed before T5c holds it: whole, of version 1, no keys.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_table_cell_index SET s = NULL WHERE table_id = $1`, tb.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE data_tables SET cell_index_version = 1 WHERE id = $1`, tb.Id); err != nil {
		t.Fatal(err)
	}

	who := accessBusiness.Subject{UserUUID: owner.UserID.String()}
	byItem := query.Spec{Scope: query.Scope{Kind: query.ScopeTable, ID: tb.Id.String()}, Sorts: []query.Sort{{Field: "field:" + item.Id.String()}}}
	byCount := query.Spec{Scope: byItem.Scope, Sorts: []query.Sort{{Field: "field:" + count.Id.String()}}}
	firstPlan := func(spec query.Spec) string {
		t.Helper()
		res, err := query.Queries.Run(ctx, who, spec, query.Page{Size: 2})
		if err != nil || res.Next == "" {
			t.Fatalf("a first window: %v %+v", err, res)
		}
		return plan(t, res.Next)
	}
	answers := func() rowQuery.Answers {
		t.Helper()
		a, err := rowQuery.AnswersFor(ctx, tb.Id)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	if a := answers(); !a.Indexed || a.SortsText {
		t.Errorf("before the sweep, the bundle says %+v", a)
	}
	if b, err := dataTable.GetBundle(ctx, tb.Id, owner); err != nil || b.RowQuery == nil || *b.RowQuery != answers() {
		t.Errorf("the bundle's row query: %+v %v", b, err)
	}
	if p := firstPlan(byItem); p != "worked" {
		t.Errorf("a text sort before the sweep was answered by %q, from entries with no keys", p)
	}
	sameAsTheEvaluator(ctx, t, owner, tb.Id, byItem, 2)
	if p := firstPlan(byCount); p != "walk" {
		t.Errorf("a number sort before the sweep was answered by %q, not the index", p)
	}

	// The sweep finds the table, and makes it whole again.
	waiting, err := model.TablesWithoutIndex(ctx, 100, rowQuery.IndexVersion)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range waiting {
		found = found || id == tb.Id
	}
	if !found {
		t.Fatalf("the sweep doesn't find a table of an older version: %v", waiting)
	}
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatal(err)
	}
	if a := answers(); !a.Indexed || !a.SortsText {
		t.Errorf("after the sweep, the bundle says %+v", a)
	}
	if p := firstPlan(byItem); p != "walk" {
		t.Errorf("a text sort after the sweep was answered by %q, not the index", p)
	}
	sameAsTheEvaluator(ctx, t, owner, tb.Id, byItem, 2)
	byItem.Sorts[0].Desc = true
	sameAsTheEvaluator(ctx, t, owner, tb.Id, byItem, 3)
	if waiting, err := model.TablesWithoutIndex(ctx, 100, rowQuery.IndexVersion); err != nil || len(waiting) != 0 {
		t.Errorf("after the sweep, tables still waiting: %v %v", waiting, err)
	}
}
