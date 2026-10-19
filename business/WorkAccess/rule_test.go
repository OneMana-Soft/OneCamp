package business

import (
	"context"
	"errors"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

var allNeeds = []Need{NeedView, NeedComment, NeedCreateTask, NeedEditTask, NeedDeleteTask, NeedLogTime, NeedPostUpdate, NeedAddFiles, NeedManage, NeedManagePeople, NeedArchive}

// The project rule, role by role, with members doing the work (the default):
// viewers read, commenters comment, members do the work, admins run the
// project, and its team's admins steward it without reading it.
func TestTheProjectRuleByRole(t *testing.T) {
	work := map[Need]bool{NeedView: true, NeedComment: true, NeedCreateTask: true, NeedEditTask: true, NeedDeleteTask: true, NeedLogTime: true, NeedPostUpdate: true, NeedAddFiles: true}
	allowed := map[string]map[Need]bool{
		"admin": {NeedView: true, NeedComment: true, NeedCreateTask: true, NeedEditTask: true, NeedDeleteTask: true, NeedLogTime: true,
			NeedPostUpdate: true, NeedAddFiles: true, NeedManage: true, NeedManagePeople: true, NeedArchive: true},
		"member":    work,
		"commenter": {NeedView: true, NeedComment: true},
		"viewer":    {NeedView: true},
		"steward":   {NeedManagePeople: true, NeedArchive: true},
		"outsider":  {},
	}
	standings := map[string]Standing{
		"admin":     {ProjectUUID: "p", Role: RoleAdmin, EditWho: EditMembers},
		"member":    {ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers},
		"commenter": {ProjectUUID: "p", Role: RoleCommenter, EditWho: EditMembers},
		"viewer":    {ProjectUUID: "p", Role: RoleViewer, EditWho: EditMembers},
		"steward":   {ProjectUUID: "p", Steward: true},
		"outsider":  {ProjectUUID: "p"},
	}
	for name, s := range standings {
		for _, n := range allNeeds {
			if got := s.Can(n); got != allowed[name][n] {
				t.Errorf("%s, need %d: %v, want %v", name, n, got, allowed[name][n])
			}
		}
	}
}

// A task, by role, by policy and by whose it is: a member changes any task,
// or with "Members on tasks assigned to them" only those; with "Admins only"
// none (today's rule before roles). A member deletes only the tasks they
// made; an admin any.
func TestTheTaskRuleByPolicy(t *testing.T) {
	theirs, assigned, made := TaskFacts{}, TaskFacts{Assignee: true}, TaskFacts{Creator: true}
	for _, c := range []struct {
		role   Role
		policy string
		facts  TaskFacts
		edit   bool
		delete bool
	}{
		{RoleMember, EditMembers, theirs, true, false},
		{RoleMember, EditMembers, made, true, true},
		{RoleMember, EditAssigned, theirs, false, false},
		{RoleMember, EditAssigned, assigned, true, false},
		{RoleMember, EditAssigned, made, false, true},
		{RoleMember, EditAdmins, assigned, false, false},
		{RoleMember, EditAdmins, made, false, false},
		{RoleAdmin, EditAdmins, theirs, true, true},
		{RoleAdmin, EditAssigned, theirs, true, true},
		{RoleCommenter, EditMembers, made, false, false},
		{RoleViewer, EditMembers, assigned, false, false},
	} {
		s := Standing{ProjectUUID: "p", Role: c.role, EditWho: c.policy}
		if got := s.CanOnTask(NeedEditTask, c.facts); got != c.edit {
			t.Errorf("%s with %s, %+v: edits %v, want %v", c.role, c.policy, c.facts, got, c.edit)
		}
		if got := s.CanOnTask(NeedDeleteTask, c.facts); got != c.delete {
			t.Errorf("%s with %s, %+v: deletes %v, want %v", c.role, c.policy, c.facts, got, c.delete)
		}
	}
	// With "Admins only", members still make no tasks, as before roles; with
	// "assigned", they make them.
	if (Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAdmins}).Can(NeedCreateTask) {
		t.Error("a member makes tasks under Admins only")
	}
	if !(Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}).Can(NeedCreateTask) {
		t.Error("a member makes no tasks under Members on tasks assigned to them")
	}
}

// Who reads a task by the rule is who reads it by the task read rule
// (domain/Task TaskReader.MayRead), for every way of standing to it.
func TestATasksReadersAreTheTaskReadRules(t *testing.T) {
	for _, inProject := range []bool{true, false} {
		for _, member := range []bool{true, false} {
			for _, admin := range []bool{true, false} {
				for _, assignee := range []bool{true, false} {
					for _, creator := range []bool{true, false} {
						if !inProject && (member || admin) {
							continue
						}
						s := Standing{}
						if inProject {
							s.ProjectUUID = "p"
						}
						switch {
						case admin:
							s.Role = RoleAdmin
						case member:
							s.Role = RoleMember
						}
						want := taskDomain.TaskReader{Member: member, Admin: admin, Assignee: assignee, Creator: creator}.MayRead(inProject)
						if got := s.CanOnTask(NeedView, TaskFacts{Assignee: assignee, Creator: creator}); got != want {
							t.Errorf("in project %v, member %v, admin %v, assignee %v, creator %v: %v, want %v", inProject, member, admin, assignee, creator, got, want)
						}
					}
				}
			}
		}
	}
}

// A task's assignee outside its project reads it and logs time on it, and
// does nothing else with it; a task in no project is changed by nobody.
func TestATasksAssigneeOutsideItsProjectOnlyReads(t *testing.T) {
	outside := Standing{ProjectUUID: "p"}
	f := TaskFacts{Assignee: true}
	for n, want := range map[Need]bool{NeedView: true, NeedLogTime: true, NeedComment: false, NeedEditTask: false, NeedDeleteTask: false} {
		if got := outside.CanOnTask(n, f); got != want {
			t.Errorf("need %d: %v, want %v", n, got, want)
		}
	}
	if (Standing{}).CanOnTask(NeedEditTask, TaskFacts{Creator: true}) {
		t.Error("a task in no project is changed by nobody, its maker included")
	}
	if got := outside.TaskLevel(f); got != accessBusiness.LevelView {
		t.Errorf("level %s, want view", got)
	}
}

// The resolver's levels follow the rule.
func TestLevelsFollowTheRule(t *testing.T) {
	for s, want := range map[Standing]accessBusiness.Level{
		{ProjectUUID: "p", Role: RoleAdmin}:                         accessBusiness.LevelFull,
		{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}:  accessBusiness.LevelEdit,
		{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}: accessBusiness.LevelEdit,
		{ProjectUUID: "p", Role: RoleMember, EditWho: EditAdmins}:   accessBusiness.LevelComment,
		{ProjectUUID: "p", Role: RoleCommenter}:                     accessBusiness.LevelComment,
		{ProjectUUID: "p", Role: RoleViewer}:                        accessBusiness.LevelView,
		{ProjectUUID: "p", Steward: true}:                           accessBusiness.LevelNone,
		{ProjectUUID: "p"}:                                          accessBusiness.LevelNone,
	} {
		if got := s.ProjectLevel(); got != want {
			t.Errorf("%+v: %s, want %s", s, got, want)
		}
	}
	member := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}
	if member.TaskLevel(TaskFacts{}) != accessBusiness.LevelComment || member.TaskLevel(TaskFacts{Assignee: true}) != accessBusiness.LevelEdit {
		t.Error("with assigned, a member edits their own tasks and comments on the rest")
	}
}

// A refusal is ErrNotAllowed and says, in words, who can or what they can do.
func TestARefusalSaysWhoCan(t *testing.T) {
	for _, c := range []struct {
		s     Standing
		need  Need
		facts TaskFacts
		words string
	}{
		{Standing{ProjectUUID: "p", Role: RoleViewer}, NeedComment, TaskFacts{}, "You can see this project but not comment in it."},
		{Standing{ProjectUUID: "p", Role: RoleCommenter}, NeedEditTask, TaskFacts{}, "You can see this task but not change it."},
		{Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}, NeedEditTask, TaskFacts{}, "In this project you can change only the tasks assigned to you."},
		{Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAdmins}, NeedEditTask, TaskFacts{}, "Only this project's admins change its tasks."},
		{Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, NeedDeleteTask, TaskFacts{}, "You can delete only the tasks you made. Ask one of the project's admins to delete this one."},
		{Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, NeedManage, TaskFacts{}, "Only the project's admins can change its settings."},
		{Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, NeedArchive, TaskFacts{}, "Only the project's admins and its team's admins can archive it."},
	} {
		err := c.s.CheckTask(c.need, c.facts)
		if !errors.Is(err, ErrNotAllowed) || err.Error() != c.words {
			t.Errorf("%+v need %d: %v", c.s, c.need, err)
		}
	}
	if err := (Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}).CheckTask(NeedEditTask, TaskFacts{}); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// fakeRoles stands in for the role rows and the policy.
func fakeRoles(t *testing.T, rows map[uuid.UUID]string, policy string) {
	t.Helper()
	oldRoles, oldWho := rolesIn, editWho
	t.Cleanup(func() { rolesIn, editWho = oldRoles, oldWho })
	rolesIn = func(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		out := map[uuid.UUID]string{}
		for _, id := range ids {
			if r, ok := rows[id]; ok {
				out[id] = r
			}
		}
		return out, nil
	}
	editWho = func(context.Context, string) string { return policy }
}

// A person's role is the lower of the graph's and their row's; with no row,
// the graph's; and nobody is in a project without its edge, whatever a row
// says.
func TestARoleIsTheLowerOfTheGraphsAndTheRows(t *testing.T) {
	demoted, promoted, plain, removed := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fakeRoles(t, map[uuid.UUID]string{demoted: "viewer", promoted: "admin", removed: "admin"}, EditMembers)
	got, err := OfMany(context.Background(), uuid.NewString(), []*dgraphStruct.DgraphProject{
		{Uuid: demoted.String(), IsProjectAdmin: 1, IsProjectMember: 1},
		{Uuid: promoted.String(), IsProjectMember: 1},
		{Uuid: plain.String(), IsProjectAdmin: 1},
		{Uuid: removed.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]Role{demoted: RoleViewer, promoted: RoleMember, plain: RoleAdmin, removed: RoleNone} {
		if s := got[id.String()]; s.Role != want {
			t.Errorf("%s: %q, want %q", id, s.Role, want)
		}
	}
	if s := got[plain.String()]; s.EditWho != EditMembers {
		t.Errorf("the policy wasn't read for a project they're in: %+v", s)
	}
	none, err := OfMany(context.Background(), "not a person", []*dgraphStruct.DgraphProject{{Uuid: plain.String(), IsProjectAdmin: 1}})
	if err != nil || none[plain.String()].Role != RoleNone {
		t.Errorf("someone who isn't a person: %+v %v", none, err)
	}
}

// Of reads the standing the graph gives when no row says otherwise: an admin
// edge an admin, a member edge a member, the team's admin count a steward,
// the deletion an archive.
func TestOfReadsTheGraphsStanding(t *testing.T) {
	fakeRoles(t, nil, EditMembers)
	ctx := context.Background()
	for _, c := range []struct {
		p    dgraphStruct.DgraphProject
		want Standing
	}{
		{dgraphStruct.DgraphProject{Uuid: "P1", IsProjectAdmin: 1, IsProjectMember: 1}, Standing{ProjectUUID: "p1", Role: RoleAdmin, EditWho: EditMembers}},
		{dgraphStruct.DgraphProject{Uuid: "p3", IsProjectMember: 1}, Standing{ProjectUUID: "p3", Role: RoleMember, EditWho: EditMembers}},
		{dgraphStruct.DgraphProject{Uuid: "p4", Team: &dgraphStruct.DgraphTeam{IsAdmin: 1}}, Standing{ProjectUUID: "p4", Steward: true}},
	} {
		p := c.p
		got, err := Of(ctx, uuid.NewString(), &p)
		if err != nil || got != c.want {
			t.Errorf("%+v: %+v %v, want %+v", c.p, got, err, c.want)
		}
	}
}

// What the app shows follows the rule: a member under "assigned" changes
// their own tasks, deletes the ones they made, and runs nothing.
func TestAccessShowsTheRule(t *testing.T) {
	a := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}.Access()
	if a.Role != "member" || !a.CreateTasks || a.EditTasks != TasksAssigned || a.DeleteTasks != TasksOwn || a.Manage || a.ManagePeople || !a.LogTime {
		t.Errorf("%+v", a)
	}
	if a := (Standing{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditAdmins}).Access(); a.EditTasks != TasksAll || a.DeleteTasks != TasksAll || !a.Manage {
		t.Errorf("an admin: %+v", a)
	}
	if a := (Standing{ProjectUUID: "p", Role: RoleViewer}).Access(); !a.View || a.Comment || a.EditTasks != TasksNone || a.DeleteTasks != TasksNone {
		t.Errorf("a viewer: %+v", a)
	}
	ta := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}.TaskAccess(TaskFacts{Assignee: true})
	if !ta.Edit || ta.Delete || !ta.Comment {
		t.Errorf("their assigned task: %+v", ta)
	}
}

// Who makes projects in a team is the workspace's "Who can make projects": the
// team's admins always, everyone in it when the workspace says so, and nobody
// in a team that's archived or that they aren't in.
func TestWhoMakesProjectsInATeam(t *testing.T) {
	gone := time.Now()
	for _, c := range []struct {
		name string
		team *dgraphStruct.DgraphTeam
		who  string
		want bool
	}{
		{"its admin, admins making them", &dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1}, MakeTeamAdmins, true},
		{"its member, admins making them", &dgraphStruct.DgraphTeam{IsMember: 1}, MakeTeamAdmins, false},
		{"its member, everyone in it making them", &dgraphStruct.DgraphTeam{IsMember: 1}, MakeTeamMembers, true},
		{"its admin, everyone in it making them", &dgraphStruct.DgraphTeam{IsAdmin: 1}, MakeTeamMembers, true},
		{"someone outside it", &dgraphStruct.DgraphTeam{}, MakeTeamMembers, false},
		{"its admin, a value that isn't one", &dgraphStruct.DgraphTeam{IsAdmin: 1}, "everyone", true},
		{"its member, a value that isn't one", &dgraphStruct.DgraphTeam{IsMember: 1}, "everyone", false},
		{"an archived team", &dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1, DeletedAt: &gone}, MakeTeamMembers, false},
		{"no team", nil, MakeTeamMembers, false},
	} {
		if got := MayMakeProjects(c.team, c.who); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// An API token (the public API, MCP) acts in a project as at most one of its
// members: its owner's admin powers stay theirs to use in the app, a
// project's and a team's as well as the workspace's (e4e1684e). Read from the
// request itself: the token's id the middleware sets.
func TestATokenActsAsAtMostAMember(t *testing.T) {
	fakeRoles(t, nil, EditAdmins)
	token := context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString())
	admin := dgraphStruct.DgraphProject{Uuid: "p1", IsProjectAdmin: 1, IsProjectMember: 1}
	stewarded := dgraphStruct.DgraphProject{Uuid: "p2", Team: &dgraphStruct.DgraphTeam{IsAdmin: 1}}

	inApp, err := Of(context.Background(), uuid.NewString(), &admin)
	if err != nil || !inApp.Can(NeedEditTask) || !inApp.Can(NeedManage) {
		t.Fatalf("the admin in the app: %+v %v", inApp, err)
	}
	got, err := Of(token, uuid.NewString(), &admin)
	if err != nil || got.Role != RoleMember || got.Can(NeedManage) || got.Can(NeedManagePeople) || got.Can(NeedArchive) {
		t.Errorf("the admin's token: %+v %v, want a member's standing", got, err)
	}
	// With "Admins only", a member changes no task, so neither does the token.
	if got.Can(NeedEditTask) || got.Can(NeedCreateTask) {
		t.Errorf("the admin's token changes tasks in an admins-only project: %+v", got)
	}
	if s, _ := Of(token, uuid.NewString(), &stewarded); s.Steward || s.Can(NeedManagePeople) || s.Can(NeedArchive) {
		t.Errorf("a team admin's token stewards the project: %+v", s)
	}
	if a := got.Access(); a.Role != "member" || a.Manage || a.ManagePeople || a.Archive {
		t.Errorf("what the token is told it may do: %+v", a)
	}
}

// A team admin's token makes projects only where everyone in the team may.
func TestATokenMakesProjectsAsTheTeamsMembersDo(t *testing.T) {
	old := makeWho
	t.Cleanup(func() { makeWho = old })
	token := context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString())
	team := &dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1}
	makeWho = func(context.Context) string { return MakeTeamAdmins }
	if !CanMakeProjects(context.Background(), team) {
		t.Error("the team's admin in the app makes no project")
	}
	if CanMakeProjects(token, team) {
		t.Error("the team admin's token makes projects where only its admins may")
	}
	if team.IsAdmin != 1 {
		t.Error("the team read for them was changed")
	}
	makeWho = func(context.Context) string { return MakeTeamMembers }
	if !CanMakeProjects(token, team) {
		t.Error("the token makes none where everyone in the team may")
	}
}

// A job a token queued runs later, on a worker, with no token id on its
// context: only the mark the job kept says how its asker asked
// (helpers.WithActsThroughToken, restored by askedForJob). Work Access reads
// it as it reads a token, so the run stands in a project as a member, and
// makes projects as the team's members do.
func TestAJobATokenQueuedActsAsAtMostAMember(t *testing.T) {
	fakeRoles(t, nil, EditAdmins)
	old := makeWho
	t.Cleanup(func() { makeWho = old })
	makeWho = func(context.Context) string { return MakeTeamAdmins }
	worker := helpers.WithActsThroughToken(context.Background())
	admin := dgraphStruct.DgraphProject{Uuid: "p1", IsProjectAdmin: 1, IsProjectMember: 1}
	if got, err := Of(worker, uuid.NewString(), &admin); err != nil || got.Role != RoleMember || got.Can(NeedManage) {
		t.Errorf("a job a token queued stands as the project's admin: %+v %v", got, err)
	}
	if CanMakeProjects(worker, &dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1}) {
		t.Error("a job a token queued makes projects as the team's admin")
	}
}
