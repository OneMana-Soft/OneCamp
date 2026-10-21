package router

import (
	workflowController "github.com/akashc777/OneCamp/controllers/Workflow"
)

// mountProjectRules is package 9 R1's routes (business/Workflow
// projectRules.go), beside today's /project routes: members read a project's
// rules and their runs and undo a run's changes they may make by hand; the
// project's admins keep the rules.
func mountProjectRules(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/rules", workflowController.ListProjectRules)
	rt.Project.Post("/{project_uuid}/rules", workflowController.CreateProjectRule)
	rt.Project.Get("/{project_uuid}/rules/runs", workflowController.ListProjectRuleRuns)
	rt.Project.Post("/{project_uuid}/rules/runs/{run_id}/undo", workflowController.UndoProjectRuleRun)
	rt.Project.Post("/{project_uuid}/rules/{rule_id}", workflowController.UpdateProjectRule)
	rt.Project.Post("/{project_uuid}/rules/{rule_id}/active", workflowController.SetProjectRuleActive)
	rt.Project.Post("/{project_uuid}/rules/{rule_id}/delete", workflowController.DeleteProjectRule)
}
