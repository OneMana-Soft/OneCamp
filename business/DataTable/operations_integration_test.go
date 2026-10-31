//go:build integration

package business

// Tables S1 step 5 (design/tables-vs-airtable/PROGRAM.md §15): an operation
// is several change sets a person undoes as one, as an import of more rows
// than one change set holds.
//
// Run: integ_rel.sh <tree> <log> '^(TestAnOperationIsUndoneAndRedoneWhole|TestAnUndoStoppedMidwayIsFinishedByTheNext)$' with PKGS='./business/DataTable/'

import (
	"fmt"
	"testing"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// An undo of an operation's change set, the latest or one named, takes all
// of the operation's, and leaves a change outside it; a redo brings them all
// back, and an undo after it takes them again. Another person's undo can't
// reach them.
func TestAnOperationIsUndoneAndRedoneWhole(t *testing.T) {
	w := newRelationWorld(t)
	tb, name := w.table("Imported")
	rows := func() int {
		t.Helper()
		got, err := model.ListRows(w.ctx, tb.Id, 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if _, err := Batch(w.ctx, tb.Id, BatchInput{Create: []RowInput{{Values: map[string]any{name.Id.String(): "before"}}}}, w.me); err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	var sets []uuid.UUID
	for b := range 3 {
		in := BatchInput{Kind: string(rowLedger.ChangeImport), Operation: op}
		for i := range 100 {
			in.Create = append(in.Create, RowInput{Values: map[string]any{name.Id.String(): fmt.Sprintf("row %d.%d", b, i)}})
		}
		res, err := Batch(w.ctx, tb.Id, in, w.me)
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, res.ChangeSet)
	}
	if n := rows(); n != 301 {
		t.Fatalf("%d rows before the undo, want 301", n)
	}
	if _, err := Undo(w.ctx, tb.Id, &sets[1], w.member); err == nil {
		t.Error("another person undid the import")
	}

	undone, err := Undo(w.ctx, tb.Id, nil, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 1 || undone.Reversed != sets[2] || undone.Kind != string(rowLedger.ChangeImport) {
		t.Fatalf("one undo left %d rows (want the one from before), reversing %v as %s", n, undone.Reversed, undone.Kind)
	}
	if _, err := Redo(w.ctx, tb.Id, w.me); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 301 {
		t.Fatalf("one redo brought back %d rows, want 301", n)
	}
	if _, err := Undo(w.ctx, tb.Id, nil, w.me); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 1 {
		t.Fatalf("an undo after the redo left %d rows, want 1", n)
	}

	// Named, a change set of the operation takes the rest with it.
	op = uuid.New()
	sets = sets[:0]
	for b := range 2 {
		res, err := Batch(w.ctx, tb.Id, BatchInput{Kind: string(rowLedger.ChangeImport), Operation: op,
			Create: []RowInput{{Values: map[string]any{name.Id.String(): fmt.Sprintf("again %d", b)}}}}, w.me)
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, res.ChangeSet)
	}
	if _, err := Undo(w.ctx, tb.Id, &sets[0], w.me); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 1 {
		t.Errorf("undoing the operation's first change set left %d rows, want 1", n)
	}
}

// Each reversal of an operation commits on its own, so an undo can stop
// midway: a reversal fails, or the request is cut off. The next undo
// finishes it, however many change sets it reversed first (the reversals and
// the sets they undid fill LatestToUndo's window two to one, so past half of
// it the window no longer reaches the rest), and one redo brings it all back.
func TestAnUndoStoppedMidwayIsFinishedByTheNext(t *testing.T) {
	w := newRelationWorld(t)
	tb, name := w.table("Imported")
	rows := func() int {
		t.Helper()
		got, err := model.ListRows(w.ctx, tb.Id, 1000, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	op := uuid.New()
	var sets []uuid.UUID
	for b := range 60 {
		res, err := Batch(w.ctx, tb.Id, BatchInput{Kind: string(rowLedger.ChangeImport), Operation: op,
			Create: []RowInput{{Values: map[string]any{name.Id.String(): fmt.Sprintf("row %d", b)}}}}, w.me)
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, res.ChangeSet)
	}
	// A field's change set can't be undone from the table, so the 55th
	// reversal, newest first, fails.
	kind := func(set uuid.UUID, k rowLedger.ChangeKind) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, `UPDATE data_table_change_sets SET kind = $2 WHERE id = $1`, set, string(k)); err != nil {
			t.Fatal(err)
		}
	}
	kind(sets[5], rowLedger.ChangeField)
	if _, err := Undo(w.ctx, tb.Id, nil, w.me); err == nil {
		t.Fatal("the undo went through a change set it can't reverse")
	}
	if n := rows(); n != 6 {
		t.Fatalf("the undo stopped with %d rows left, want 6", n)
	}
	kind(sets[5], rowLedger.ChangeImport)

	if _, err := Undo(w.ctx, tb.Id, nil, w.me); err != nil {
		t.Fatalf("the next undo didn't finish the import: %v", err)
	}
	if n := rows(); n != 0 {
		t.Fatalf("the next undo left %d rows, want none", n)
	}
	var undos, ops int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx, `SELECT COUNT(*), COUNT(DISTINCT operation) FROM data_table_change_sets
		WHERE table_id = $1 AND kind = 'undo'`, tb.Id).Scan(&undos, &ops); err != nil {
		t.Fatal(err)
	}
	if undos != 60 || ops != 1 {
		t.Errorf("the import's undo is %d change sets in %d operations, want 60 in one", undos, ops)
	}
	if _, err := Redo(w.ctx, tb.Id, w.me); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 60 {
		t.Errorf("one redo brought back %d rows, want 60", n)
	}
}
