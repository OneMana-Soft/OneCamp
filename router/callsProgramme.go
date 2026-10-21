package router

import (
	"github.com/go-chi/chi/v5"
)

// The calls programme's routes (design/calls-vs-zoom/PROGRAM.md §3.6 and §9),
// on the docs, chat, calendar and projects programmes' pattern: router.go
// calls mountCallsProgramme once, after every other route, and each calls
// package adds ONE line to mountCallsProgramme, keeping its own routes in
// router/calls<Area>.go as func mount<Area>(rt callsRouters).
//
// THE ROUTERS CARRY THEIR PROTECTION (publicSurface_test.go): callsRouters
// holds only the member group. A public route (the lobby's guest page, a
// webinar's registration) is pinned one at a time in publicSurface_test.go
// with its own rate limit, and its line goes in router.go.
type callsRouters struct {
	// Member is the root behind the member group's middleware, for the
	// programme's own paths (/calls/...).
	Member chi.Router
}

// newCallsRouters builds the routers a calls package may mount on. root is
// the top-level router, used only to open the member group.
func newCallsRouters(root chi.Router) callsRouters {
	return callsRouters{Member: membersOnly(root)}
}

// mountCallsProgramme mounts every merged package of the calls programme: one
// line per package, in merge order, and nothing else.
func mountCallsProgramme(rt callsRouters) {
	mountCallSessions(rt) // CALLS-1: Call State's reads and Call Control's actions
}
