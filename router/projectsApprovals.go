package router

import (
	approvalsController "github.com/akashc777/OneCamp/controllers/Approvals"
)

// mountApprovals is package 9 R2's routes (business/Approvals): a project's
// status rules beside /project (members read, admins change), and a task's
// approval request beside /task (its readers see it, its approvers answer,
// the person who asked withdraws).
func mountApprovals(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/statusRules", approvalsController.ListStatusRules)
	rt.Project.Post("/{project_uuid}/statusRules", approvalsController.SaveStatusRule)
	rt.Project.Post("/{project_uuid}/statusRules/delete", approvalsController.DeleteStatusRule)
	rt.Task.Get("/approval/{task_uuid}", approvalsController.TaskApproval)
	rt.Task.Post("/approval/{task_uuid}/decide", approvalsController.DecideApproval)
	rt.Task.Post("/approval/{task_uuid}/withdraw", approvalsController.WithdrawApproval)
	rt.Task.Post("/approval/{task_uuid}/ask", approvalsController.AskApproval)
	// Every open request waiting for the reader's answer (R2b).
	rt.Member.Get("/approvals/waiting", approvalsController.WaitingApprovals)
}
