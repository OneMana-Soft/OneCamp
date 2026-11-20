package router

import (
	triageController "github.com/akashc777/OneCamp/controllers/Triage"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mailboxDialsMax is how often one member may test a mailbox, and how often
// save one, in UserWriteRate's 15-minute window: a test dials the mail server
// (up to 10 s to connect, 30 s a command) and a save has the poller dial it
// within a minute, so a held-down button can't make the server knock on
// someone's mail host, and saving can't be made into a way to ask which
// names resolve inside this server's network.
const mailboxDialsMax = 5

// mountTriage is package 9 R5a's routes (business/Triage): a project's
// triage queue beside /project (its people read it), and a decision beside
// /task (for those who can change the task). A project turns triage on
// through its policies (mountProjectRoles); its rota (who is on triage duty)
// is read by its people and set by its admins (linear-21 H2).
func mountTriage(rt projectsRouters) {
	rt.Project.Get("/{project_uuid}/triage", triageController.Queue)
	rt.Project.Get("/{project_uuid}/triage/rota", triageController.Rota)
	rt.Project.Put("/{project_uuid}/triage/rota", triageController.SetRota)
	// The mail door (linear-21): the project's admins connect a mailbox.
	rt.Project.Get("/{project_uuid}/triage/mailbox", triageController.Mailbox)
	rt.Project.With(customMiddleware.UserRateLimit("triage-mailbox-save", mailboxDialsMax, "Too many mailbox saves. Try again in a few minutes.", nil)).
		Put("/{project_uuid}/triage/mailbox", triageController.SetMailbox)
	rt.Project.Delete("/{project_uuid}/triage/mailbox", triageController.RemoveMailbox)
	rt.Project.With(customMiddleware.UserRateLimit("triage-mailbox-test", mailboxDialsMax, "Too many mailbox tests. Try again in a few minutes.", nil)).
		Post("/{project_uuid}/triage/mailbox/test", triageController.TestMailbox)
	rt.Task.Post("/triage/{task_uuid}/decide", triageController.Decide)
}
