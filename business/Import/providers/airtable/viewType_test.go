package airtable

import "testing"

// An Airtable view keeps its layout where OneCamp has one: kanban as a board,
// and a calendar, a gallery, a list, a timeline and a Gantt as themselves. A
// form, which OneCamp has no layout for yet, comes as a grid.
func TestAnAirtableViewKeepsItsLayout(t *testing.T) {
	for in, want := range map[string]string{
		"grid": "grid", "kanban": "board", "calendar": "calendar", "gallery": "gallery",
		"list": "list", "timeline": "timeline", "gantt": "gantt", "form": "grid", "block": "grid", "": "grid",
	} {
		if got := viewType(in); got != want {
			t.Errorf("viewType(%q) = %q; want %q", in, got, want)
		}
	}
}
