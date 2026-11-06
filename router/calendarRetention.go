package router

import (
	retentionController "github.com/akashc777/OneCamp/controllers/Retention"
)

// mountCalendarRetention is the governance trail's H2 (business/Retention):
// retention rules and holds, calendar events their first kind, for workspace
// admins only (/admin). Every change, search and export is recorded before it
// is answered; the people on a hold are never told.
func mountCalendarRetention(rt calendarRouters) {
	rt.Admin.Get("/retention/rules", retentionController.ListRules)
	rt.Admin.Get("/retention/rules/{kind}/preview", retentionController.PreviewRule)
	rt.Admin.Put("/retention/rules/{kind}", retentionController.SetRule)
	rt.Admin.Get("/retention/holds", retentionController.ListHolds)
	rt.Admin.Post("/retention/holds", retentionController.PlaceHold)
	rt.Admin.Patch("/retention/holds/{id}", retentionController.ChangeHold)
	rt.Admin.Post("/retention/holds/{id}/release", retentionController.ReleaseHold)
	rt.Admin.Get("/retention/holds/{id}/items/{kind}", retentionController.SearchHold)
	rt.Admin.Get("/retention/holds/{id}/export/{kind}", retentionController.ExportHold)
}
