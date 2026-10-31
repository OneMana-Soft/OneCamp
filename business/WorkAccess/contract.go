// Package business (WorkAccess) is Work Access
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #86; A1 built it): what
// someone may do with a project and its work, decided in one place.
//
// The rule is Standing.Can and Standing.CanOnTask (rule.go), over the facts Of,
// OfMany and OfTask read once for a person: their role in the project, whether
// they run its team, the project's "Who can change tasks" policy, and for a
// task whether it is theirs. Every check on a project or a task asks it: the
// app's handlers through controllers/ProjectAccess, business/Task's TaskFor,
// the public API, the agents' and MCP's tools; and so do the Access Resolver's
// work kinds (kinds.go), so anything asking accessBusiness.Rules agrees.
//
// ROLES. Per project: an admin runs it (settings, statuses, fields, forms,
// cycles, rates, invoices, members, archive); a member does the work (makes,
// changes, moves and completes tasks, deletes the ones they made, logs time,
// posts updates, adds files); a commenter reads and comments; a viewer reads.
// The admins of the project's team steward it without being in it: they
// manage its people and archive it, and read none of its work. A task's
// assignee reads it wherever it is.
//
// WHERE ROLES LIVE. The graph's member and admin edges say who is in a project
// (every read of work counts them); project_roles (migration 320) says each
// person's role. A person is in a project only with an edge; their role is
// the lower of the graph's (an admin edge admin, a member edge member) and
// their row's, and with no row the graph's. So a path that writes only the
// graph gives D-PJ-2's default, member, and a role written in one store and
// not the other errs low (business/Project roles.go writes both, lower first;
// the projects.roles.parity sweep settles what a failure leaves).
package business

import (
	"context"
	"strings"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// Need is what an action needs of someone on a project or one of its tasks.
type Need int

const (
	// NeedView reads the project and its work.
	NeedView Need = iota + 1
	// NeedComment comments on its tasks and reacts to comments.
	NeedComment
	// NeedCreateTask makes a task or a sub-task in it.
	NeedCreateTask
	// NeedEditTask changes a task: any of its fields, its place on the board,
	// its dependencies, cycle and repeat, its files and its GitHub link.
	NeedEditTask
	// NeedDeleteTask deletes or restores a task.
	NeedDeleteTask
	// NeedLogTime logs one's own time on a task.
	NeedLogTime
	// NeedPostUpdate posts a project update.
	NeedPostUpdate
	// NeedAddFiles adds files to the project.
	NeedAddFiles
	// NeedManage changes the project's settings: its name, statuses, fields,
	// forms, cycles, rates, invoices, templates, sharing and policies.
	NeedManage
	// NeedManagePeople changes who is in the project and their roles.
	NeedManagePeople
	// NeedArchive archives or restores the project.
	NeedArchive
)

// Role is someone's place in a project.
type Role string

const (
	// RoleNone is someone outside the project.
	RoleNone Role = ""
	// RoleViewer reads.
	RoleViewer Role = roleModel.Viewer
	// RoleCommenter reads and comments.
	RoleCommenter Role = roleModel.Commenter
	// RoleMember does the work.
	RoleMember Role = roleModel.Member
	// RoleAdmin runs the project.
	RoleAdmin Role = roleModel.Admin
)

// rank orders the roles, lowest first.
var rank = map[Role]int{RoleNone: 0, RoleViewer: 1, RoleCommenter: 2, RoleMember: 3, RoleAdmin: 4}

// ParseRole reads a role as the API spells it; ok is false for anything else.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	if _, ok := rank[r]; !ok || r == RoleNone {
		return RoleNone, false
	}
	return r, true
}

// Lower is the lower of two roles. Pure.
func Lower(a, b Role) Role {
	if rank[a] < rank[b] {
		return a
	}
	return b
}

// Who may change a project's tasks (projects.edit.who).
const (
	EditMembers  = "members"
	EditAssigned = "assigned"
	EditAdmins   = "admins"
)

// Standing is how one person stands to one project: what the rule decides by,
// read once (Of).
type Standing struct {
	// ProjectUUID is the project's id; "" for a task in no project.
	ProjectUUID string
	// Role is their role in the project.
	Role Role
	// Steward is whether they are an admin of the project's team, who manage
	// its people and archive it without being in it.
	Steward bool
	// Archived is whether the project is archived.
	Archived bool
	// EditWho is the project's "Who can change tasks": EditMembers,
	// EditAssigned or EditAdmins.
	EditWho string
}

// TaskFacts is how one person stands to one task beyond its project.
type TaskFacts struct {
	// Assignee and Creator say whether the task is assigned to them, and
	// whether they made it.
	Assignee, Creator bool
	// Deleted is whether the task is in the trash.
	Deleted bool
	// Restricted is whether its project keeps the task to a few people
	// (WA2), and Granted whether they are one of the people let in by name.
	// Its project's admins, its assignee and its creator are always let in.
	Restricted, Granted bool
}

// Seams for tests: the role rows, the restrictions and the policies.
var (
	rolesIn        = roleModel.RolesIn
	restrictionsOf = restrictionModel.OfMany
	editWho        = func(ctx context.Context, projectUUID string) string {
		return policyBusiness.Policies.ChoiceIn(ctx, policyBusiness.ProjectsEditWho, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: projectUUID})
	}
	primeEditWho = func(ctx context.Context, projects []string) {
		policyBusiness.Policies.PrimeScopes(ctx, policyBusiness.ScopeProject, projects)
	}
	makeWho = func(ctx context.Context) string {
		return policyBusiness.Policies.Choice(ctx, policyBusiness.ProjectsCreateWho)
	}
)

// CanMakeProjects is whether someone may make projects in team, read for
// them, by the workspace's "Who can make projects" (MayMakeProjects). With an
// API token they make them as one of the team's members: a team admin's
// power stays theirs to use in the app, as asToken keeps a project's.
func CanMakeProjects(ctx context.Context, team *dgraphStruct.DgraphTeam) bool {
	if team != nil && viaToken(ctx) {
		member := *team
		member.IsAdmin = 0
		team = &member
	}
	return MayMakeProjects(team, makeWho(ctx))
}

// Of is how the person (userUUID) stands to p, as read for them: p's member
// and admin counts are theirs (every project query reads them for its
// reader), and its team's admin count when the query read it. Their role row
// is one read; a read that fails is an error, never a guess.
func Of(ctx context.Context, userUUID string, p *dgraphStruct.DgraphProject) (Standing, error) {
	if p == nil {
		return Standing{}, nil
	}
	got, err := OfMany(ctx, userUUID, []*dgraphStruct.DgraphProject{p})
	if err != nil {
		return Standing{}, err
	}
	return got[strings.ToLower(p.Uuid)], nil
}

// OfMany is Of for many projects read for one person, by lower-case project
// id, with one read of their role rows.
func OfMany(ctx context.Context, userUUID string, ps []*dgraphStruct.DgraphProject) (map[string]Standing, error) {
	out, ids := graphStandings(ps)
	me, err := uuid.Parse(userUUID)
	if err != nil {
		// Someone who isn't a person stands nowhere.
		for key := range out {
			out[key] = Standing{ProjectUUID: key, Archived: out[key].Archived}
		}
		return out, nil
	}
	if len(ids) > 0 {
		rows, err := rolesIn(ctx, me, ids)
		if err != nil {
			return nil, err
		}
		lowerBy(out, rows)
	}
	// Their projects' own policies, read together (PR1, the BE audit's #2).
	var in []string
	for key, s := range out {
		if s.Role != RoleNone {
			in = append(in, key)
		}
	}
	if len(in) > 1 {
		primeEditWho(ctx, in)
	}
	for key, s := range out {
		if s.Role != RoleNone {
			s.EditWho = editWho(ctx, key)
		}
		if viaToken(ctx) {
			s = s.asToken()
		}
		out[key] = s
	}
	return out, nil
}

// viaToken is whether the call acts through an API token: the public API, MCP,
// or an agent run a token asked for, a job a token queued included, whose
// worker has no token id but the mark the job kept (helpers.ActsThroughToken,
// the one answer every token-reachable rule asks). A seam for tests.
var viaToken = func(ctx context.Context) bool { return helpers.ActsThroughToken(ctx) }

// asToken is standing s for a request an API token made: at most a member's
// role, and no team admin's stewardship. A token carries none of its owner's
// admin powers (e4e1684e, the owner's decision of 16 Oct): neither the
// workspace's (TokenSubjectOf) nor a project's or a team's, which the
// programme's design doesn't give tokens either, so the stricter rule holds.
// Those stay for the owner to use in the app. Pure.
func (s Standing) asToken() Standing {
	if s.Role == RoleAdmin {
		s.Role = RoleMember
	}
	s.Steward = false
	return s
}

// OfTask is how the person stands to t, as read for them (by uuid userUUID
// and graph uid userUID): its project's standing, and the task's facts, its
// restriction included.
func OfTask(ctx context.Context, userUUID, userUID string, t *dgraphStruct.DgraphTask) (Standing, TaskFacts, error) {
	facts, err := FactsFor(ctx, userUUID, userUID, t)
	if err != nil {
		return Standing{}, facts, err
	}
	if t == nil || t.Project == nil || (t.Project.Uuid == "" && t.Project.Uid == "") {
		return Standing{}, facts, nil
	}
	s, err := Of(ctx, userUUID, t.Project)
	return s, facts, err
}

// FactsFor is how the person (by uuid and graph uid) stands to t beyond its
// project, its restriction included: one read of it. Every decision on a task
// asks this, or FactsForMany, so none can miss that a task is restricted.
func FactsFor(ctx context.Context, userUUID, userUID string, t *dgraphStruct.DgraphTask) (TaskFacts, error) {
	if t == nil {
		return TaskFacts{}, nil
	}
	many, err := FactsForMany(ctx, userUUID, userUID, t)
	if err != nil {
		return TaskFacts{}, err
	}
	return many[strings.ToLower(t.Uuid)], nil
}

// FactsForMany is FactsFor for many tasks, by lower-case task uuid, with one
// read of their restrictions.
func FactsForMany(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) (map[string]TaskFacts, error) {
	out := make(map[string]TaskFacts, len(ts))
	var ids []uuid.UUID
	for _, t := range ts {
		if t == nil {
			continue
		}
		out[strings.ToLower(t.Uuid)] = FactsOf(t, userUID)
		if id, err := uuid.Parse(t.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	restrictions, err := restrictionsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	me, _ := uuid.Parse(userUUID)
	var through []string // restricted with a task above it, not let in by name
	for id, r := range restrictions {
		key := id.String()
		f, ok := out[key]
		if !ok || !r.Restricted {
			continue
		}
		f.Restricted = true
		for _, g := range r.Grants {
			if me != uuid.Nil && g == me {
				f.Granted = true
			}
		}
		if !f.Granted && r.Via != uuid.Nil && me != uuid.Nil {
			through = append(through, key)
		}
		out[key] = f
	}
	// Whoever may see the task one is restricted with (its assignee, its
	// creator) is let into it: the one filter says so (models/taskread).
	if len(through) > 0 {
		tr, err := taskread.ForReader(ctx, me.String())
		if err != nil {
			return nil, err
		}
		for _, key := range through {
			if !tr.Hides(key) {
				f := out[key]
				f.Granted = true
				out[key] = f
			}
		}
	}
	return out, nil
}

// FactsOf is how the person (by graph uid) stands to t beyond its project, as
// the graph alone says: not whether it is restricted, which FactsFor reads.
// Pure.
func FactsOf(t *dgraphStruct.DgraphTask, userUID string) TaskFacts {
	if t == nil {
		return TaskFacts{}
	}
	is := func(u *dgraphStruct.DgraphUser) bool { return userUID != "" && u != nil && u.Uid == userUID }
	return TaskFacts{Assignee: is(t.Assignee), Creator: is(t.CreatedBy), Deleted: helpers.IsSoftDeleted(t.DeletedAt)}
}

// dgraphStanding is the standing the graph alone says: an admin edge makes an
// admin, a member edge a member. Pure.
func dgraphStanding(p *dgraphStruct.DgraphProject) Standing {
	if p == nil {
		return Standing{}
	}
	s := Standing{
		ProjectUUID: strings.ToLower(p.Uuid),
		Steward:     p.Team != nil && p.Team.IsAdmin > 0,
		Archived:    helpers.IsSoftDeleted(p.DeletedAt),
	}
	switch {
	case p.IsProjectAdmin > 0:
		s.Role = RoleAdmin
	case p.IsProjectMember > 0:
		s.Role = RoleMember
	}
	return s
}

// graphStandings is each project's standing as read from the graph for one
// person, by lower-case project id, and the projects they have a role in,
// whose role rows may say less. Pure.
func graphStandings(ps []*dgraphStruct.DgraphProject) (map[string]Standing, []uuid.UUID) {
	out := make(map[string]Standing, len(ps))
	var ids []uuid.UUID
	for _, p := range ps {
		if p == nil {
			continue
		}
		s := dgraphStanding(p)
		out[s.ProjectUUID] = s
		if s.Role == RoleNone {
			continue
		}
		if id, err := uuid.Parse(s.ProjectUUID); err == nil {
			ids = append(ids, id)
		}
	}
	return out, ids
}

// lowerBy lowers each standing with a role to what the person's role row in
// its project says, when that says less (a row never raises one). Pure.
func lowerBy(out map[string]Standing, rows map[uuid.UUID]string) {
	for id, row := range rows {
		key := id.String()
		s, ok := out[key]
		if !ok {
			continue
		}
		if r, ok := ParseRole(row); ok && s.Role != RoleNone {
			s.Role = Lower(s.Role, r)
		}
		out[key] = s
	}
}
