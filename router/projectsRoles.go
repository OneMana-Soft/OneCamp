package router

import (
	noticeController "github.com/akashc777/OneCamp/controllers/Notice"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
)

// mountProjectRoles is A1's routes (Work Access): someone's role in a
// project, and the project's own "Who can change tasks", beside today's
// /project routes; a workspace admin making themselves a team's admin (and
// so its projects' steward); and the one-time notices, which tell the admins
// that members now change tasks.
func mountProjectRoles(rt projectsRouters) {
	rt.Project.Post("/{project_uuid}/members/{user_uuid}/role", projectController.SetMemberRole)
	rt.Project.Get("/{project_uuid}/policies", projectController.ProjectPolicies)
	rt.Project.Put("/{project_uuid}/policies/{key}", projectController.SetProjectPolicy)
	rt.Admin.Post("/teams/{team_uuid}/admins/me", teamController.MakeMeTeamAdmin)
	rt.Member.Get("/notices", noticeController.Notices)
	rt.Member.Post("/notices/{key}/seen", noticeController.DismissNotice)
}
