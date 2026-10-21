package business

// People's roles in a project (Work Access, design/projects-vs-asana-jira/
// PROGRAM.md A1 and D-PJ-2), and how the two stores stay in step.
//
// The graph's member and admin edges say who is in a project, for every read
// of its work; project_roles says how much each may do. Work Access takes the
// lower of the two (business/WorkAccess), so every write here goes lower
// first, and a write that fails halfway leaves the person with less, never
// more:
//
//   - a lower role: the row first, then the admin edge off when they were an
//     admin;
//   - a higher one: the row first (which counts for nothing until the edge
//     agrees), then the admin edge on for an admin;
//   - joining: the row, then the edges; leaving: the edges, then the row.
//
// The projects.roles.parity sweep settles what a failure leaves, toward the
// lower: a row for someone off the project goes, a missing row is filled from
// the graph, and where the row is lower than the admin edge the edge goes.
// Every path that changes membership comes through this package
// (AddMemberToProject, AddAdminMemberToProject, RemoveMemberFromProject,
// SetRole, CreateProject; leaving the team takes the rows in business/Team),
// so imports, templates and the AI's projects keep both without a line of
// their own.

import (
	"context"
	"errors"
	"maps"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	"github.com/google/uuid"
)

// ErrNotInProject is a role for someone who isn't in the project.
var ErrNotInProject = errors.New("they aren't in this project")

// Seams for tests: the role rows and the admin edge.
var (
	setRow        = roleModel.Set
	deleteRow     = roleModel.Delete
	rowsOf        = roleModel.Roles
	rowsOfMany    = roleModel.RolesOfMany
	keepRows      = roleModel.Keep
	addAdminEdge  = addAdminEdgeOnly
	dropAdminEdge = domain.RemoveAdminMemberFromProject
	projectsPage  = domain.ProjectMembershipsPage
	// invalidateMemberships drops a person's cached profile, which lists
	// their projects (search and the sidebar read it).
	invalidateMemberships = userDomain.InvalidateUserMemberships
)

// SetRole gives someone in the project a role: the row first, then the
// admin edge on or off to match (the file's comment). by is who changed it,
// projectUID the project's graph uid, person their graph node (uid and
// uuid), and adminEdge whether they are on the project's admin edge now.
func SetRole(ctx context.Context, by uuid.UUID, projectUUID uuid.UUID, projectUID string, person *dgraphStruct.DgraphUser, adminEdge bool, to workAccess.Role) error {
	if person == nil || person.Uid == "" {
		return ErrNotInProject
	}
	who, err := uuid.Parse(person.Uuid)
	if err != nil {
		return ErrNotInProject
	}
	if err := setRow(ctx, projectUUID, who, string(to), by); err != nil {
		return err
	}
	switch {
	case adminEdge && to != workAccess.RoleAdmin:
		if err := dropAdminEdge(ctx, projectUID, person.Uid); err != nil {
			return err
		}
	case !adminEdge && to == workAccess.RoleAdmin:
		if err := addAdminEdge(ctx, projectUUID, person.Uid); err != nil {
			return err
		}
	default:
		return nil
	}
	// Their cached profile lists their projects and whether they run them.
	invalidateMemberships(ctx, person.Uuid)
	return nil
}

// FillRoles sets each listed member's ProjectRole on p (read with its members
// flagged IsAdmin from its admins), as Work Access decides it: the lower of
// the graph's and their row's.
func FillRoles(ctx context.Context, p *dgraphStruct.DgraphProject) error {
	if p == nil {
		return nil
	}
	id, err := uuid.Parse(p.Uuid)
	if err != nil {
		return nil
	}
	rows, err := rowsOf(ctx, id)
	if err != nil {
		return err
	}
	for _, m := range p.Members {
		if m == nil {
			continue
		}
		role := workAccess.RoleMember
		if m.IsAdmin {
			role = workAccess.RoleAdmin
		}
		if mid, err := uuid.Parse(m.Uuid); err == nil {
			if row, ok := workAccess.ParseRole(rows[mid]); ok {
				role = workAccess.Lower(role, row)
			}
		}
		m.ProjectRole = string(role)
	}
	return nil
}

// --------------------------------------------------------------- the sweep

// ParityEvery is how often the roles are brought into step with the graph.
const ParityEvery = time.Hour

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "projects.roles.parity", Every: ParityEvery, Run: func(ctx context.Context) error {
		_, err := SettleRoles(ctx)
		return err
	}})
}

// SettleRoles brings every project's role rows into step with its graph
// edges, a page of projects at a time, and answers how many rows and edges
// it changed. Toward the lower (the file's comment): a missing row is the
// graph's role, a row for someone off the project goes, and where the row is
// lower than an admin edge the edge goes. A page that can't be read stops the
// pass (the next one starts over); a project that can't be settled is logged
// and skipped.
func SettleRoles(ctx context.Context) (changed int, err error) {
	after := ""
	for {
		page, err := projectsPage(ctx, after, domain.MembershipsPage)
		if err != nil {
			return changed, err
		}
		// The page's rows in one read (PR3, the BE audit's #34).
		ids := make([]uuid.UUID, 0, len(page))
		for _, p := range page {
			if id, err := uuid.Parse(p.Uuid); err == nil {
				ids = append(ids, id)
			}
		}
		rowsByProject, err := rowsOfMany(ctx, ids)
		if err != nil {
			return changed, err
		}
		for _, p := range page {
			n, err := settleProjectWith(ctx, p, rowsByProject)
			changed += n
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/Project SettleRoles: project %s: %+v", p.Uuid, err)
			}
		}
		if len(page) < domain.MembershipsPage {
			return changed, nil
		}
		after = page[len(page)-1].Uid
	}
}

// settleProjectWith is SettleRoles for one project, read with its members
// and admins, its rows among rowsByProject. A project already in step writes
// nothing.
func settleProjectWith(ctx context.Context, p *dgraphStruct.DgraphProject, rowsByProject map[uuid.UUID]map[uuid.UUID]string) (int, error) {
	id, err := uuid.Parse(p.Uuid)
	if err != nil {
		return 0, nil
	}
	rows := rowsByProject[id]
	if rows == nil {
		rows = map[uuid.UUID]string{}
	}
	want, dropEdges := settled(p, rows)
	if len(dropEdges) == 0 && maps.Equal(want, rows) {
		return 0, nil
	}
	changed := 0
	for _, uid := range dropEdges {
		if err := dropAdminEdge(ctx, p.Uid, uid); err != nil {
			return changed, err
		}
		changed++
	}
	n, err := keepRows(ctx, id, want)
	return changed + n, err
}

// settled is what a project's rows should say, by person, from its graph
// edges and its rows now, and whose admin edge goes because their row is
// lower. Pure.
func settled(p *dgraphStruct.DgraphProject, rows map[uuid.UUID]string) (map[uuid.UUID]string, []string) {
	want := map[uuid.UUID]string{}
	byUUID := map[uuid.UUID]*dgraphStruct.DgraphUser{}
	admin := map[uuid.UUID]bool{}
	in := func(users []*dgraphStruct.DgraphUser, isAdmin bool) {
		for _, u := range users {
			if u == nil {
				continue
			}
			id, err := uuid.Parse(u.Uuid)
			if err != nil {
				continue
			}
			byUUID[id] = u
			if isAdmin {
				admin[id] = true
			}
		}
	}
	in(p.Members, false)
	in(p.Admins, true)
	var dropEdges []string
	for id, u := range byUUID {
		graph := workAccess.RoleMember
		if admin[id] {
			graph = workAccess.RoleAdmin
		}
		role := graph
		if row, ok := workAccess.ParseRole(rows[id]); ok {
			role = workAccess.Lower(graph, row)
		}
		if graph == workAccess.RoleAdmin && role != workAccess.RoleAdmin {
			dropEdges = append(dropEdges, u.Uid)
		}
		want[id] = string(role)
	}
	return want, dropEdges
}
