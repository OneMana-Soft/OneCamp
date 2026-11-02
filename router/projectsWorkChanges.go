package router

import (
	workFeedController "github.com/akashc777/OneCamp/controllers/WorkFeed"
)

// mountWorkChanges is WCL-1's: the Work Change Log's read, what changed in
// the caller's work since their place in it, for members; each change only
// to whoever may see its task now. See controllers/WorkFeed.
func mountWorkChanges(rt projectsRouters) {
	rt.Member.Get("/work/changes", workFeedController.Changes)
}
