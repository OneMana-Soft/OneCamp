package router

import (
	taskKeysController "github.com/akashc777/OneCamp/controllers/TaskKeys"
)

// mountTaskKeys is A8K's routes (Task Keys #94, Work Taxonomy #95): a
// project's key and its types beside today's /project routes, a task's type
// and finding a task by its key beside today's /task routes.
func mountTaskKeys(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/key", taskKeysController.ProjectKey)
	rt.Project.Put("/{project_uuid}/key", taskKeysController.SetProjectKey)
	rt.Project.Get("/{project_uuid}/types", taskKeysController.ProjectTypes)
	rt.Project.Post("/{project_uuid}/types", taskKeysController.CreateProjectType)
	rt.Project.Put("/{project_uuid}/types/{type_id}", taskKeysController.UpdateProjectType)
	rt.Project.Delete("/{project_uuid}/types/{type_id}", taskKeysController.ArchiveProjectType)
	rt.Task.Post("/type", taskKeysController.SetTaskType)
	rt.Task.Get("/resolve/{ref}", taskKeysController.ResolveTask)
}
