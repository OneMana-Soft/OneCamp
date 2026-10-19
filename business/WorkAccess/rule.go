package business

import (
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// at is whether standing s holds at least role r in its project. Pure.
func (s Standing) at(r Role) bool { return s.Role != RoleNone && rank[s.Role] >= rank[r] }

// membersWork is whether the project lets its members do the work at all:
// "Admins only" keeps tasks with its admins, as before roles. Pure.
func (s Standing) membersWork() bool { return s.EditWho != EditAdmins }

// Can is whether someone with standing s may do n on the project. The one
// rule for a project; CanOnTask is the one for a task. Pure.
func (s Standing) Can(n Need) bool {
	admin := s.Role == RoleAdmin
	switch n {
	case NeedView:
		return s.at(RoleViewer)
	case NeedComment:
		return s.at(RoleCommenter)
	case NeedCreateTask:
		return admin || (s.at(RoleMember) && s.membersWork())
	case NeedEditTask:
		// On the project: may they change its tasks at all (all of them, or
		// those assigned to them)? CanOnTask decides a task.
		return admin || (s.at(RoleMember) && s.membersWork())
	case NeedDeleteTask:
		return admin || (s.at(RoleMember) && s.membersWork())
	case NeedLogTime, NeedPostUpdate, NeedAddFiles:
		return s.at(RoleMember)
	case NeedManagePeople, NeedArchive:
		return admin || s.Steward
	}
	// NeedManage, and anything else: its admins.
	return admin
}

// CanOnTask is whether someone with standing s in a task's project, and facts f
// about the task, may do n on it. Pure.
//
// A member changes any task, or with "Members on tasks assigned to them" only
// those; deletes and restores only the tasks they made. A task's assignee
// outside the project reads it and logs time on it; a task in no project is
// its maker's to read.
func (s Standing) CanOnTask(n Need, f TaskFacts) bool {
	switch n {
	case NeedView, NeedLogTime:
		if f.Assignee || (s.ProjectUUID == "" && f.Creator) {
			return true
		}
	}
	if s.ProjectUUID == "" {
		return false
	}
	admin := s.Role == RoleAdmin
	switch n {
	case NeedEditTask:
		if admin {
			return true
		}
		return s.Can(NeedEditTask) && (s.EditWho != EditAssigned || f.Assignee)
	case NeedDeleteTask:
		return admin || (s.Can(NeedDeleteTask) && f.Creator)
	}
	return s.Can(n)
}

// Reasons, as codes the web app words (accessBusiness.Decision.Reasons).
const (
	ReasonAdmin     = "admin"
	ReasonMember    = "member"
	ReasonCommenter = "commenter"
	ReasonViewer    = "viewer"
	ReasonAssignee  = "assignee"
	ReasonCreator   = "creator"
	ReasonOwner     = "owner"
)

// ProjectLevel is the Access Resolver's level for standing s on the project:
// full to run it, edit to do its work, comment, view or none, by the rule.
// Pure.
func (s Standing) ProjectLevel() accessBusiness.Level {
	return levelBy(func(n Need) bool { return s.Can(n) })
}

// TaskLevel is the Access Resolver's level for standing s and facts f on a
// task, by the rule. Pure.
func (s Standing) TaskLevel(f TaskFacts) accessBusiness.Level {
	return levelBy(func(n Need) bool { return s.CanOnTask(n, f) })
}

// levelBy maps what the rule allows onto the resolver's levels. Pure.
func levelBy(can func(Need) bool) accessBusiness.Level {
	switch {
	case can(NeedManage):
		return accessBusiness.LevelFull
	case can(NeedEditTask):
		return accessBusiness.LevelEdit
	case can(NeedComment):
		return accessBusiness.LevelComment
	case can(NeedView):
		return accessBusiness.LevelView
	}
	return accessBusiness.LevelNone
}

// reasonsFor names where standing s comes from, for a task with facts f (nil
// for the project itself). Pure.
func reasonsFor(s Standing, f *TaskFacts) []string {
	var out []string
	if s.Role != RoleNone {
		out = append(out, string(s.Role))
	}
	if f != nil && f.Assignee {
		out = append(out, ReasonAssignee)
	}
	if f != nil && f.Creator && s.ProjectUUID == "" {
		out = append(out, ReasonCreator)
	}
	return out
}

// Who may make projects in a team (projects.create.who).
const (
	MakeTeamAdmins  = "team_admins"
	MakeTeamMembers = "team_members"
)

// MayMakeProjects is whether someone may make projects in team, as read for
// them (its admin and member counts are theirs), when the workspace's "Who
// can make projects" is who: the team's admins always, and with
// MakeTeamMembers everyone in it. A deleted team makes nothing. Pure.
func MayMakeProjects(team *dgraphStruct.DgraphTeam, who string) bool {
	if team == nil || helpers.IsSoftDeleted(team.DeletedAt) {
		return false
	}
	return team.IsAdmin > 0 || (who == MakeTeamMembers && team.IsMember > 0)
}
