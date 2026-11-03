package router

import (
	taskTemplateController "github.com/akashc777/OneCamp/controllers/TaskTemplate"
	"github.com/go-chi/chi/v5"
)

// taskTemplateRoutes mounts task templates (linear-09; business/TaskTemplate)
// on the project router: the workspace's, which every member uses and its
// admins keep, and each project's, which whoever may add the project's tasks
// uses and its admins keep. A task is made from either through the
// project's make route. tests/integration reaches them through Routes(), as the app does.
func taskTemplateRoutes(r chi.Router) {
	r.Get("/task-templates", taskTemplateController.ListWorkspace)
	r.Post("/task-templates", taskTemplateController.AddToWorkspace)
	r.Get("/task-templates/{template_id}", taskTemplateController.GetFromWorkspace)
	r.Post("/task-templates/{template_id}", taskTemplateController.ChangeInWorkspace)
	r.Post("/task-templates/{template_id}/delete", taskTemplateController.DeleteFromWorkspace)
	r.Post("/task-templates/{template_id}/restore", taskTemplateController.RestoreToWorkspace)
	r.Get("/{project_uuid}/task-templates", taskTemplateController.ListInProject)
	r.Post("/{project_uuid}/task-templates", taskTemplateController.AddToProject)
	r.Get("/{project_uuid}/task-templates/{template_id}", taskTemplateController.GetInProject)
	r.Post("/{project_uuid}/task-templates/{template_id}", taskTemplateController.ChangeInProject)
	r.Post("/{project_uuid}/task-templates/{template_id}/delete", taskTemplateController.DeleteFromProject)
	r.Post("/{project_uuid}/task-templates/{template_id}/restore", taskTemplateController.RestoreToProject)
	r.Post("/{project_uuid}/task-templates/{template_id}/make", taskTemplateController.MakeInProject)
}
