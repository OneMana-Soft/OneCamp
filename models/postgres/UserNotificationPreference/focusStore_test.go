package models

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// namesOneStore is whether q reads focus time from one store throughout:
// calendar_events with its own columns, or the one store's events with its.
func namesOneStore(q string) bool {
	oldCols := strings.Contains(q, ".is_focus") || strings.Contains(q, ".start_time") || strings.Contains(q, ".end_time")
	newCols := strings.Contains(q, ".kind = 'focus'") || strings.Contains(q, ".start_at") || strings.Contains(q, ".end_at")
	if strings.Contains(q, "FROM calendar_events ") {
		return oldCols && !newCols
	}
	return strings.Contains(q, "FROM events ") && newCols && !oldCols
}

// flipCalendarMoved makes the move's record answer differently each time it
// is asked, as it can while a query is built at the moment the move is
// recorded. It needs the one store on.
func flipCalendarMoved(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	moved := false
	helpers.SetCalendarMoved(func() bool { moved = !moved; return moved })
	// With the store on, no probe and one that says moved answer alike.
	t.Cleanup(func() { helpers.SetCalendarMoved(func() bool { return true }) })
}

// A read of focus time asks where the events are once, and builds the whole
// query from that answer. Asked part by part, a query built while the move was
// being recorded named calendar_events with the one store's columns, and
// every read of a person's preferences failed then.
func TestAFocusTimeQueryReadsOneStore(t *testing.T) {
	flipCalendarMoved(t)
	for i := 0; i < 4; i++ {
		if q := allColumns(); !namesOneStore(q) {
			t.Fatalf("read %d mixes the stores:\n%s", i, q)
		}
	}
	for _, c := range []struct {
		store, table string
	}{{"graph", "calendar_events fe"}, {"postgres", "events fe"}} {
		t.Setenv("CALENDAR_STORE", c.store)
		helpers.SetCalendarMoved(func() bool { return true })
		if got := FocusStoreNow().Events("fe"); got != c.table {
			t.Errorf("with the %s store, focus time is read from %q", c.store, got)
		}
	}
}
