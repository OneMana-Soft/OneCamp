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
		"tables.trash.restore": "[tables.row.delete]",
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
