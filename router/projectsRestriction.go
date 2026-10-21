package router

import (
	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
)

// mountTaskRestriction is WA2's routes (Work Access #86, restricted tasks):
// who besides its project's admins, assignee and creator may see a task,
// beside today's /task routes. A task is named by its id or its key.
func mountTaskRestriction(rt projectsRouters) {
	rt.Task.Get("/{task_ref}/restriction", taskRestrictionController.Restriction)
	rt.Task.Put("/{task_ref}/restriction", taskRestrictionController.SetRestriction)
	// PR2: one change to many tasks, in one request.
	rt.Task.Post("/bulk", taskBulkController.Bulk)
	rt.Task.Post("/createTasks", taskBulkController.CreateTasks)
}
