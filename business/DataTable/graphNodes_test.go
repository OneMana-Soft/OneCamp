package business

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/google/uuid"
)

// The work graph hears of the rows a change set touched that link work
// (DG14): rows made, deleted or brought back, rows whose links changed, and
// rows whose relation or people cells changed; not a cell elsewhere, or a
// row moved.
func TestTheGraphHearsOfTheRowsThatLinkWork(t *testing.T) {
	rel, people, text := uuid.NewString(), uuid.NewString(), uuid.NewString()
	made, linked, cited, typed, moved, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cs := &rowLedger.ChangeSet{Changes: []rowLedger.Change{
		{Row: made, Field: rowLedger.WholeRow, After: json.RawMessage(`{"id":"x"}`)},
		{Row: linked, Field: rowLedger.LinksPrefix + rel},
		{Row: cited, Field: people, After: json.RawMessage(`[{"id":"u"}]`)},
		{Row: cited, Field: rel, After: json.RawMessage(`[]`)},
		{Row: typed, Field: text, After: json.RawMessage(`"hello"`)},
		{Row: moved, Field: rowLedger.FieldPosition},
		{Row: gone, Field: rowLedger.WholeRow},
	}}
	links := func(field string) bool { return field == rel || field == people }
	got := graphRowsOf(cs, links)
	want := []uuid.UUID{made, linked, cited, gone}
	if !slices.Equal(got, want) {
		t.Errorf("the graph hears of %v, want %v (made, linked, cited, gone; each once)", got, want)
	}
	if rows := graphRowsOf(&rowLedger.ChangeSet{Changes: []rowLedger.Change{{Row: typed, Field: text}}}, links); len(rows) != 0 {
		t.Errorf("a text cell's change told the graph of %v", rows)
	}
}

// A change set's entries are added in its transaction and delivered once it
// commits, once; one whose write rolled back is let go.
func TestTheGraphsEntriesWaitForTheirChangeSetToCommit(t *testing.T) {
	saved := GraphNodes
	t.Cleanup(func() { GraphNodes = saved })
	var delivered []int64
	GraphNodes.Committed = func(ids []int64) { delivered = append(delivered, ids...) }

	cs := uuid.New()
	holdGraph(cs, []int64{1, 2, 3}, time.Now())
	if len(delivered) != 0 {
		t.Fatalf("delivered %v before the change set committed", delivered)
	}
	graphLedgerCommitted(context.Background(), &rowLedger.ChangeSet{ID: cs})
	if !slices.Equal(delivered, []int64{1, 2, 3}) {
		t.Errorf("delivered %v once the change set committed, want its 3 entries", delivered)
	}
	graphLedgerCommitted(context.Background(), &rowLedger.ChangeSet{ID: cs})
	if len(delivered) != 3 {
		t.Errorf("a change set's entries were delivered twice: %v", delivered)
	}
	// A change set held past a minute (its write rolled back) is let go when another is held.
	rolledBack := uuid.New()
	holdGraph(rolledBack, []int64{9}, time.Now().Add(-2*heldGraphFor))
	holdGraph(uuid.New(), []int64{10}, time.Now())
	if got := releaseGraph(rolledBack); len(got) != 0 {
		t.Errorf("a rolled back change set's entries were kept: %v", got)
	}
}
