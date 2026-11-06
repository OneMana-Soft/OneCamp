package router

import (
	dashboardController "github.com/akashc777/OneCamp/controllers/Dashboard"
	"github.com/go-chi/chi/v5"
)

// dashboardRoutes mounts dashboards (B9-b2; business/Dashboard) on the
// project router: a person's own and the workspace's are listed and made at
// /dashboards, a project's at /{project_uuid}/dashboards, and every other
// route names the dashboard alone, its scope read from it.
// tests/integration reaches them through Routes(), as the app does.
func dashboardRoutes(r chi.Router) {
	r.Get("/dashboards", dashboardController.List)
	r.Post("/dashboards", dashboardController.Add)
	r.Get("/{project_uuid}/dashboards", dashboardController.ListInProject)
	r.Post("/{project_uuid}/dashboards", dashboardController.AddToProject)
	r.Get("/dashboards/{dashboard_id}", dashboardController.Get)
	r.Post("/dashboards/{dashboard_id}", dashboardController.Change)
	r.Get("/dashboards/{dashboard_id}/data", dashboardController.Data)
	r.Post("/dashboards/{dashboard_id}/delete", dashboardController.Delete)
	r.Post("/dashboards/{dashboard_id}/restore", dashboardController.Restore)
	r.Post("/dashboards/{dashboard_id}/arrange", dashboardController.Arrange)
	r.Post("/dashboards/{dashboard_id}/widgets", dashboardController.AddWidget)
	r.Post("/dashboards/{dashboard_id}/widgets/{widget_id}", dashboardController.ChangeWidget)
	r.Post("/dashboards/{dashboard_id}/widgets/{widget_id}/delete", dashboardController.RemoveWidget)
	r.Post("/dashboards/{dashboard_id}/widgets/{widget_id}/restore", dashboardController.RestoreWidget)
}
