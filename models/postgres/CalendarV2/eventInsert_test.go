package models

import (
	"slices"
	"strings"
	"testing"
)

// An insert names each column once and gives each a value, in one order, and
// writes the organiser's "Send the recap to guests" as the row has it (594):
// the new series "this and following" makes is inserted from the old one's
// row, and must keep it.
func TestAnInsertWritesEveryColumnItNamesWithTheRecapSwitch(t *testing.T) {
	var cols []string
	for _, c := range strings.Split(insertColumns, ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	args := insertArgs(&Event{RecapToGuests: true})
	if len(cols) != insertColumnCount || len(args) != insertColumnCount {
		t.Fatalf("%d columns, %d values, insertColumnCount %d", len(cols), len(args), insertColumnCount)
	}
	i := slices.Index(cols, "recap_to_guests")
	if i < 0 {
		t.Fatal("an insert doesn't write recap_to_guests")
	}
	if on, ok := args[i].(bool); !ok || !on {
		t.Errorf("recap_to_guests is written as %v, want the row's true", args[i])
	}
	if !strings.Contains(eventColumns, "e.recap_to_guests") {
		t.Error("a read doesn't return recap_to_guests")
	}
}
