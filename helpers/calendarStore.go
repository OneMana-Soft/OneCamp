package helpers

import (
	"os"
	"strings"
	"sync/atomic"
)

// CalendarInPostgres reports whether the calendar's one store is in use
// (calendar programme A1, D-CAL-1): every event read and written in
// Postgres's events tables, and the Dgraph Event nodes and calendar_events
// rows left as they were. It is the default since v2.74.0 (CAL-DEFAULT):
// CALENDAR_STORE=graph keeps the two old stores, for an install that has to
// wait, and every calendar path then runs as it did before the store existed.
// Anything else, "postgres" or nothing at all, is the one store.
//
// Read on every call, never kept from start-up: a local run loads .env after
// the packages are initialised, and a flag read then would say the default.
func CalendarInPostgres() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(CalendarStoreEnv)), CalendarOldStores)
}

const (
	// CalendarStoreEnv names the variable that keeps the calendar's old stores.
	CalendarStoreEnv = "CALENDAR_STORE"
	// CalendarOldStores is its one value that does: the graph's Event nodes
	// and calendar_events, as before the one store.
	CalendarOldStores = "graph"
)

// calendarMoved says, without waiting, whether this install's events have been
// moved into the one store; business/Calendar sets it (SetCalendarMoved).
var calendarMoved atomic.Pointer[func() bool]

// SetCalendarMoved is how the calendar says whether the move into its one
// store has finished on this install. The probe must answer at once.
func SetCalendarMoved(probe func() bool) {
	calendarMoved.Store(&probe)
}

// CalendarReadsFromStore reports whether calendar reads come from the one
// store: it is on, and this install's events have been moved into it. While
// they are being moved, every reader (the calendar, busy times, statuses, the
// push gate's focus time, the workload's time off) still reads the two old
// stores, which no write changes meanwhile: writes wait for the move
// (business/Calendar ErrMoving). All of them switch together when the move is
// recorded. Writes ask CalendarInPostgres. Without the calendar linked (a
// package's own tests) the switch alone says.
func CalendarReadsFromStore() bool {
	if !CalendarInPostgres() {
		return false
	}
	if probe := calendarMoved.Load(); probe != nil {
		return (*probe)()
	}
	return true
}
