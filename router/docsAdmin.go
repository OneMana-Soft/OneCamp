package router

import (
	departedController "github.com/akashc777/OneCamp/controllers/Departed"
)

// mountDocsAdmin is C6-2's admin routes: the work of people who left (the
// list, handing it on, turning their links off) and how many links a shorter
// link lifetime would stop. Admins only; every change is audited.
func mountDocsAdmin(rt docsRouters) {
	rt.Admin.Get("/departed", departedController.ListDeparted)
	rt.Admin.Post("/departed/{user_uuid}/handover", departedController.HandOver)
	rt.Admin.Post("/departed/{user_uuid}/links/off", departedController.TurnOffLinks)
	rt.Admin.Get("/guest-links/stopped", departedController.CountStoppedLinks)
}
