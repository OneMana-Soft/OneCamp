package router

import (
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
)

// mountRestore is V-78's: ONE route brings back what a soft delete kept (a
// project's field, status, cycle, milestone or form, a table's form, a
// person's time entry, a draft invoice), for members; each kind checks the
// caller as its delete does. See controllers/Restore.
func mountRestore(rt projectsRouters) {
	rt.Member.Post("/restore/{kind}/{id}", restoreController.Restore)
}
