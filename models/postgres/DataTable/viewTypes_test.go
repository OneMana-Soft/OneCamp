package models

import "testing"

// A view's layout is one the app draws: B4 adds the gallery, the list, the
// timeline and the Gantt to the grid, the board, the calendar and the chart.
func TestAViewsLayoutIsOneTheAppDraws(t *testing.T) {
	for _, v := range []string{ViewGrid, ViewBoard, ViewCalendar, ViewChart, ViewGallery, ViewList, ViewTimeline, ViewGantt} {
		if !ValidViewType(v) {
			t.Errorf("%q refused; want it a layout", v)
		}
	}
	for _, v := range []string{"", "form", "kanban", "Gallery", "gantt "} {
		if ValidViewType(v) {
			t.Errorf("%q taken; want it refused", v)
		}
	}
}
