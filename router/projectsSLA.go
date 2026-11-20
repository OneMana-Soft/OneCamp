package router

import (
	slaController "github.com/akashc777/OneCamp/controllers/SLA"
)

// mountSLA is linear-24 H1's: a project's SLA settings, read by its members
// and changed by its admins (business/SLA, which this import also starts: its
// Work Change Log sink and its task read layer).
func mountSLA(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/sla", slaController.Show)
	rt.Project.Put("/{project_uuid}/sla/hours", slaController.SetHours)
	rt.Project.Post("/{project_uuid}/sla/rules", slaController.AddRule)
	rt.Project.Patch("/{project_uuid}/sla/rules/{rule_id}", slaController.ChangeRule)
	rt.Project.Delete("/{project_uuid}/sla/rules/{rule_id}", slaController.RemoveRule)
}
