package business

import (
	"fmt"
	"strings"
	"testing"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// Main's ruling A (2 Nov): a tables write is Recoverable only through ops
// published beside it (APIOps, registered as one list), and these are,
// through these. A new tables write that is Recoverable names its undo here
// too.
func TestEveryRecoverableTablesOpNamesItsPublishedUndo(t *testing.T) {
	all := APIOps()
	if err := v1api.CheckUndo(all); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"tables.rows.batch":    "[tables.row.delete tables.row.history tables.row.update tables.trash.restore]",
		"tables.row.delete":    "[tables.trash.restore]",
		"tables.row.update":    "[tables.row.history tables.row.update]",
		"tables.trash.restore": "[tables.field.delete tables.row.delete tables.view.delete]",
		// Tables H2b: a field or a view made is deleted to the trash, one
		// changed is changed back (a field's cells from their rows'
		// history), and one deleted comes back from the trash.
		"tables.field.create": "[tables.field.delete]",
		"tables.field.update": "[tables.field.update tables.row.history tables.row.update]",
		"tables.field.delete": "[tables.trash.restore]",
		"tables.fields.batch": "[tables.field.delete tables.field.update tables.row.history tables.row.update tables.trash.restore]",
		"tables.view.create":  "[tables.view.delete]",
		"tables.view.update":  "[tables.view.update]",
		"tables.view.delete":  "[tables.trash.restore]",
		"tables.views.batch":  "[tables.trash.restore tables.view.delete tables.view.update]",
		// Tables H2c: a table made is deleted to the tables' trash, one
		// renamed is renamed back, one deleted comes back and one brought
		// back is deleted again; a thread resolved is reopened, and back. An
		// edited or deleted comment isn't: its words aren't kept for the API.
		"tables.table.create":       "[tables.table.delete]",
		"tables.table.update":       "[tables.table.update]",
		"tables.table.delete":       "[tables.table.restore]",
		"tables.table.restore":      "[tables.table.delete]",
		"tables.row.thread.resolve": "[tables.row.thread.reopen]",
		"tables.row.thread.reopen":  "[tables.row.thread.resolve]",
	}
	seen := 0
	for _, op := range all {
		if !strings.HasPrefix(op.Name, "tables.") {
			continue
		}
		got := ""
		if op.Behaviour.Recoverable {
			got, seen = fmt.Sprint(op.UndoneBy), seen+1
		}
		if got != want[op.Name] {
			t.Errorf("%s is Recoverable through %q, want %q", op.Name, got, want[op.Name])
		}
	}
	if seen != len(want) {
		t.Errorf("%d tables ops are Recoverable, want %d", seen, len(want))
	}
}
