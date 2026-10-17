package router

import (
	"github.com/go-chi/chi/v5"
)

// The calendar programme's routes (design/calendar-vs-gcal/PROGRAM.md §3.6 and
// §9), on the docs and chat programmes' pattern (docsProgramme.go,
// chatProgramme.go): router.go is frozen and calls mountCalendarProgramme
// once, after every other route; each calendar package adds ONE line to
// mountCalendarProgramme, written by the integrator at its merge, and keeps
// its own routes in router/calendar<Area>.go as
// func mount<Area>(rt calendarRouters).
//
// THE ROUTERS CARRY THEIR PROTECTION (publicSurface_test.go): calendarRouters
// holds only routers behind the member group or /admin, never the root. A
// public route (A2's /rsvp/{token}, B5's booking pages, B8's feeds) is pinned
// one at a time in publicSurface_test.go with its own rate limit (CAL9), and
// the integrator adds its line to router.go at wiring.
//
// A ROUTE SWAP (A1's service behind the old /event routes, which go at A7's
// wiring) deletes the old line from router.go and registers the new handler
// on the same sub-router here: chi keeps the last handler registered for a
// method and pattern without a word.
type calendarRouters struct {
	// Member is the root behind the member group's middleware, for the
	// programme's own paths (/calendar/events, /calendar/availability).
	Member chi.Router
	// Admin is /admin: workspace admins only.
	Admin chi.Router
	// Event is router.go's /event, for route swaps and routes beside today's.
	Event chi.Router
}

// newCalendarRouters builds the routers a calendar package may mount on. root
// is the top-level router, used only to open the member group.
func newCalendarRouters(root, admin, event chi.Router) calendarRouters {
	return calendarRouters{Member: membersOnly(root), Admin: admin, Event: event}
}

// mountCalendarProgramme mounts every merged package of the calendar
// programme: one line per package, in merge order, and nothing else.
func mountCalendarProgramme(rt calendarRouters) {
	// No package has merged yet.
}
