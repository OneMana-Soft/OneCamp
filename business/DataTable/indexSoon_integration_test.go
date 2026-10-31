//go:build integration

package business

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// answeredSoon waits until Row Query answers a table's views on the server (its
// cell index whole, and sorting by text), or the wait runs out.
func answeredSoon(ctx context.Context, t *testing.T, table uuid.UUID, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		a, err := rowQuery.AnswersFor(ctx, table)
		if err == nil && a.Indexed && a.SortsText {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s isn't answered by the server 30 s on (answers %+v, err %v): its index waits for the repair sweep", what, a, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A table an import fills, and a table made by hand, are answered by the
// server at once: each one's cell index is made whole right away, not at the
// repair sweep's next pass (every 5 minutes; it never runs here). Until then
// the browser read a big table's rows itself, 10,000 at most.
func TestAFreshImportIsAnsweredWithoutTheSweep(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	lines := ImportTable{Key: "tblL", Name: "Lines", Fields: []ImportField{
		{Key: "fItem", Name: "Item", Type: model.FieldText},
		{Key: "fAmount", Name: "Amount", Type: model.FieldNumber},
	}}
	lines.Rows = func(page func([]ImportRow) error) error {
		rows := make([]ImportRow, 1200)
		for i := range rows {
			rows[i] = ImportRow{Key: fmt.Sprintf("r%d", i), Cells: map[string]any{"fItem": fmt.Sprintf("Line %d", i), "fAmount": float64(i)}}
		}
		return page(rows)
	}
	made, err := ImportBaseAs(ctx, w.me, ImportBase{Tables: []ImportTable{lines}})
	if err != nil {
		t.Fatalf("import: %v %+v", err, made.Notes)
	}
	if made.RowsMade != 1200 {
		t.Fatalf("made %d rows", made.RowsMade)
	}
	answeredSoon(ctx, t, made.Tables["tblL"], "the imported table")

	byHand, err := CreateTable(ctx, TableInput{Name: "By hand"}, w.me)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	answeredSoon(ctx, t, byHand.Id, "a table made by hand")
}

// An import into a table whose index is whole leaves the index alone: each
// row it writes is indexed as it's written, so walking the table's every row
// again was a waste (CR-64). A table not whole yet, or whole at an older
// version, is still walked.
func TestATableWholeAlreadyIsNotWalkedAgain(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tbl, err := CreateTable(ctx, TableInput{Name: "Whole"}, w.me)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	answeredSoon(ctx, t, tbl.Id, "the table")
	if indexWanted(ctx, tbl.Id) {
		t.Fatal("a table whole at this version is walked again")
	}
	if err := model.MarkCellIndexStale(ctx, tbl.Id); err != nil {
		t.Fatal(err)
	}
	if !indexWanted(ctx, tbl.Id) {
		t.Fatal("a table whose index isn't whole isn't walked")
	}
}

// A big import into a table whose index is whole is counted for the planner,
// as a walk's rows are (V-48). IndexTable has the planner count the rows and
// the cell index again once it has indexed 1,000 rows (AnalyzeRows), but an
// import into a table the index kept up with skips the walk, and the count
// with it. Until autovacuum's next look, that table's first grouped or summed
// window ran past its budget, and its summaries and groups were left out: a
// grouping the server refuses showed neither its groups nor its refusal (the
// live walk's 1,050-row import, 28 Oct).
func TestABigImportIntoAWholeTableIsCountedForThePlanner(t *testing.T) {
	w := newRelationWorld(t)
	ctx := w.ctx
	tbl, err := CreateTable(ctx, TableInput{Name: "Kept"}, w.me)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	item, err := CreateField(ctx, tbl.Id, FieldInput{Name: "Item", Type: model.FieldText}, w.me)
	if err != nil {
		t.Fatalf("field: %v", err)
	}
	answeredSoon(ctx, t, tbl.Id, "the table")
	if indexWanted(ctx, tbl.Id) {
		t.Fatal("the table isn't whole before the import")
	}
	// Nothing else counts the rows while this test watches the last count.
	if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
		t.Fatalf("background work still running 30 s on: %v", busy)
	}
	db := postgresInit.DBConn.SqlDB
	var since time.Time
	if err := db.QueryRowContext(ctx, `SELECT NOW()`).Scan(&since); err != nil {
		t.Fatal(err)
	}

	const n = 1200
	made, err := ImportBaseAs(ctx, w.me, ImportBase{Tables: []ImportTable{{Key: "Kept", Name: "Kept", Into: tbl.Id,
		Fields: []ImportField{{Key: "c1", Name: "Item", Type: model.FieldText, Into: item.Id}},
		Rows: func(page func([]ImportRow) error) error {
			rows := make([]ImportRow, n)
			for i := range rows {
				rows[i] = ImportRow{Key: fmt.Sprintf("r%d", i), Cells: map[string]any{"c1": fmt.Sprintf("Item %d", i)}}
			}
			return page(rows)
		}}}})
	if err != nil {
		t.Fatalf("import: %v %+v", err, made.Notes)
	}
	if made.RowsMade != n {
		t.Fatalf("made %d rows, want %d", made.RowsMade, n)
	}
	if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
		t.Fatalf("background work still running 30 s on: %v", busy)
	}
	// The cell index was counted again (a plain ANALYZE: autovacuum's is
	// last_autoanalyze) after the import began. The statistics reach other
	// sessions within half a second.
	counted := func() bool {
		var at sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT last_analyze FROM pg_stat_user_tables WHERE relname = 'data_table_cell_index'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at.Valid && at.Time.After(since)
	}
	for deadline := time.Now().Add(10 * time.Second); !counted(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("an import of %d rows into a table whose index is whole wasn't counted for the planner: its first grouped or summed windows run past their budget until autovacuum looks", n)
		}
	}
}
