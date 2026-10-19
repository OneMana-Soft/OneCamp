package router

import (
	"github.com/go-chi/chi/v5"
)

// The tables programme's routes (design/tables-vs-airtable/PROGRAM.md §3.6 and
// §9), on the docs, chat and calendar programmes' pattern (docsProgramme.go):
// router.go is frozen and calls mountTablesProgramme once, after every other
// route; each tables package adds ONE line to mountTablesProgramme, written by
// the integrator at its merge, and keeps its own routes in router/tables<Area>.go
// as func mount<Area>(rt tablesRouters).
//
// THE ROUTERS CARRY THEIR PROTECTION (publicSurface_test.go): tablesRouters
// holds only routers behind the member group, /admin or the public API's token,
// never the root. A route outsiders reach (B3's shared views, B5's forms, B7's
// published interface pages) is pinned one at a time in publicSurface_test.go
// with its own rate limit, and the integrator adds its line to router.go at
// wiring.
//
// A ROUTE SWAP (A2's ledger behind today's row writes, A3's windows behind the
// rows endpoint) deletes the old line from router.go and registers the new
// handler on the same sub-router here: chi keeps the last handler registered
// for a method and pattern without a word.
type tablesRouters struct {
	// Member is the root behind the member group's middleware, for the
	// programme's own paths (/bases, A5).
	Member chi.Router
	// Admin is /admin: workspace admins only.
	Admin chi.Router
	// Tables is router.go's /tables, behind the member group: for route swaps
	// and routes beside today's (/tables/{id}/rows/batch, /tables/{id}/undo).
	Tables chi.Router
	// V1 is the public API's /v1: an API token's owner, each route asserting
	// the scope it needs (customMiddleware.RequireScope), as B10's are.
	V1 chi.Router
}

// newTablesRouters builds the routers a tables package may mount on. tables,
// admin and v1 are router.go's /tables, /admin and /v1 sub-routers; root is the
// top-level router, used only to open the member group.
func newTablesRouters(root, admin, tables, v1 chi.Router) tablesRouters {
	return tablesRouters{Member: membersOnly(root), Admin: admin, Tables: tables, V1: v1}
}

// mountTablesProgramme mounts every merged package of the tables programme:
// one line per package, in merge order, and nothing else.
func mountTablesProgramme(rt tablesRouters) {
	mountRowLedger(rt) // A2
	mountRowPages(rt)  // T2 (rows as pages)
}
