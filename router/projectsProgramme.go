package router

import (
	"github.com/go-chi/chi/v5"

	// The work kinds of the Access Resolver (project, task, view, cycle, form)
	// register from business/WorkAccess's init, and the projects programme's
	// sweeps from business/WorkSweep's: imported here so the server always has
	// them, whichever projects package merges first.
	_ "github.com/akashc777/OneCamp/business/WorkAccess"
	_ "github.com/akashc777/OneCamp/business/WorkSweep"
)

// The projects programme's routes (design/projects-vs-asana-jira/PROGRAM.md
// §3.6 and §9), on the docs, chat and calendar programmes' pattern: router.go
// is frozen and calls mountProjectsProgramme once, after every other route;
// each projects package adds ONE line to mountProjectsProgramme, written by the
// integrator at its merge, and keeps its own routes in
// router/projects<Area>.go as func mount<Area>(rt projectsRouters).
//
// THE ROUTERS CARRY THEIR PROTECTION (publicSurface_test.go): projectsRouters
// holds only routers behind the member group, /admin or the public API's token
// check, never the root.
//
// A ROUTE SWAP (A1's checks behind today's /project and /task routes, A3's
// Task Writer behind the per-field task routes) deletes the old line from
// router.go and registers the new handler on the same sub-router here: chi
// keeps the last handler registered for a method and pattern without a word.
type projectsRouters struct {
	// Member is the root behind the member group's middleware, for an area
	// with a path of its own (/work, /views).
	Member chi.Router
	// Admin is /admin: workspace admins only.
	Admin chi.Router
	// Project and Task are router.go's /project and /task, for route swaps
	// and for routes that belong beside today's.
	Project chi.Router
	Task    chi.Router
	// V1 is the public API's /v1: an API token's owner, each route asserting
	// the scope it needs (customMiddleware.RequireScope).
	V1 chi.Router
}

// newProjectsRouters builds the routers a projects package may mount on. root
// is the top-level router, used only to open the member group.
func newProjectsRouters(root, admin, project, task, v1 chi.Router) projectsRouters {
	return projectsRouters{Member: membersOnly(root), Admin: admin, Project: project, Task: task, V1: v1}
}

// mountProjectsProgramme mounts every merged package of the projects
// programme: one line per package, in merge order, and nothing else.
func mountProjectsProgramme(rt projectsRouters) {
	mountProjectRoles(rt) // A1: roles, "Who can change tasks", stewardship, notices
}
