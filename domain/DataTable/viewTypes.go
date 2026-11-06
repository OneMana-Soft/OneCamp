// Package tableDomain holds a table's pure rules: checks on what a caller
// sends that need nothing read or written. The store layer
// (models/postgres/DataTable) only persists and reads, so a check that makes
// no round trip lives here, where the models take their constants from and
// any layer may call it without reaching a store.
package tableDomain

// The layouts a table's view may take.
const (
	ViewGrid     = "grid"
	ViewBoard    = "board"
	ViewCalendar = "calendar"
	ViewChart    = "chart"
	// B4's layouts: cards, a list of rows, bars across their dates, and the
	// bars with the links that say what waits on what (migration 927).
	ViewGallery  = "gallery"
	ViewList     = "list"
	ViewTimeline = "timeline"
	ViewGantt    = "gantt"
)

// ValidViewType reports whether t is a layout a view may take.
func ValidViewType(t string) bool {
	switch t {
	case ViewGrid, ViewBoard, ViewCalendar, ViewChart, ViewGallery, ViewList, ViewTimeline, ViewGantt:
		return true
	default:
		return false
	}
}
