package business

// Project + team agent tools. These mirror the EXACT permission model the
// HTTP controllers enforce, so an agent action can never do more than the user
// could do by hand:
//
//   - create_task requires the user may add tasks to the target project, by
//     Work Access's rule (their role and its "Who can change tasks"), as
//     controllers/Task/CreateTask asks. list_projects surfaces, per project,
//     the user's role so the model proposes create_task only where it will
//     succeed - and resolves a project by name.
//   - create_project requires the user may make projects in the target team
//     (the workspace's "Who can make projects", as controllers/Project/
//     CreateProject asks). list_teams surfaces, per team, whether they may, so
//     the model can resolve the team and know where a project may be created.
//
// list_projects / list_teams are read-only (auto-run in the agent loop);
// create_project is a write and stays behind the user-confirmation gate.

import (
	"context"
	"fmt"
	"strings"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// executeListProjects lists the projects the acting user belongs to, marking
// the ones they may add and change tasks in (Work Access). Read-only.
func executeListProjects(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	actingUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	dgraphUser, err := userBusiness.GetDgraphUserProjectList(ctx, actingUser.Uuid)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list projects: %w", err)
	}
	// Asked for by someone other than the sponsor: only the projects they are
	// in too. A list takes no id the runner could check, so it is narrowed here.
	asker, err := askerView(ctx)
	if err != nil {
		return "", nil, err
	}
	if dgraphUser == nil || len(dgraphUser.Projects) == 0 {
		return "You are not a member of any projects.", nil, nil
	}

	// Projects the user administers (same source the project admin UI uses).
	adminSet := map[string]bool{}
	if adminProjects, aerr := projectBusiness.GetDgraphProjectListByAdminDgraphUID(ctx, actingUser.Uid); aerr == nil {
		for _, p := range adminProjects {
			if p != nil && p.Uuid != "" {
				adminSet[p.Uuid] = true
			}
		}
	}

	// What they may do in each, by Work Access's rule (their role and the
	// project's "Who can change tasks"): one read of their roles for the lot.
	reads := make([]*dgraphStruct.DgraphProject, 0, len(dgraphUser.Projects))
	for _, p := range dgraphUser.Projects {
		if p != nil && p.Uuid != "" {
			read := &dgraphStruct.DgraphProject{Uuid: p.Uuid, IsProjectMember: 1}
			if adminSet[p.Uuid] {
				read.IsProjectAdmin = 1
			}
			reads = append(reads, read)
		}
	}
	standings, err := workAccess.OfMany(ctx, actingUser.Uuid, reads)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't read what you can do in your projects: %w", err)
	}

	var b strings.Builder
	b.WriteString("Your projects:\n")
	shown := 0
	for _, p := range dgraphUser.Projects {
		if p == nil || p.Uuid == "" {
			continue
		}
		// Skip soft-deleted/archived projects.
		if p.DeletedAt != nil && p.DeletedAt.Year() > 1970 {
			continue
		}
		if asker != nil && !asker.projects[p.Uuid] {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = "(untitled project)"
		}
		canManage := "no"
		if standings[strings.ToLower(p.Uuid)].Can(workAccess.NeedCreateTask) {
			canManage = "yes"
		}
		line := fmt.Sprintf("- %s [can create/manage tasks: %s", name, canManage)
		if p.Team != nil && strings.TrimSpace(p.Team.Name) != "" {
			line += fmt.Sprintf(", team: %s", p.Team.Name)
		}
		line += fmt.Sprintf("] (project_uuid: %s)", p.Uuid)
		b.WriteString(line)
		b.WriteString("\n")
		shown++
	}
	if shown == 0 {
		return "You are not a member of any active projects.", nil, nil
	}

	return strings.TrimSpace(b.String()), nil, nil
}

// executeReadProject returns an overview of a single project (status, team,
// the user's role, and member names). Enforces the same rule as the project-
// info endpoints: the user must be in the project. Read-only.
func executeReadProject(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	projectUUID := strings.TrimSpace(action.Params["project_uuid"])
	if projectUUID == "" {
		return "", nil, fmt.Errorf("project_uuid is required")
	}
	if _, err := uuid.Parse(projectUUID); err != nil {
		return "", nil, fmt.Errorf("invalid project UUID")
	}

	actingUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	proj, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, projectUUID, actingUser.Uid)
	if err != nil || proj == nil || proj.Uuid == "" {
		return "", nil, fmt.Errorf("project not found or you don't have access")
	}
	if !proj.ReaderIsIn() {
		return "", nil, fmt.Errorf("you must be in this project to view it")
	}
	standing, err := workAccess.Of(ctx, actingUser.Uuid, proj)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't read your role in this project: %w", err)
	}

	name := strings.TrimSpace(proj.Name)
	if name == "" {
		name = "(untitled project)"
	}
	role := string(standing.Role)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Project: %s\n", name))
	if strings.TrimSpace(proj.Status) != "" {
		b.WriteString(fmt.Sprintf("Status: %s\n", proj.Status))
	}
	if proj.Team != nil && strings.TrimSpace(proj.Team.Name) != "" {
		b.WriteString(fmt.Sprintf("Team: %s\n", proj.Team.Name))
	}
	b.WriteString(fmt.Sprintf("Your role: %s\n", role))

	// Member names (best-effort; never blocks the overview).
	if mi, mErr := projectBusiness.GetDgraphProjectMemberInfo(ctx, projectUUID, actingUser.Uid); mErr == nil && mi != nil && len(mi.Members) > 0 {
		names := make([]string, 0, len(mi.Members))
		for _, m := range mi.Members {
			if m != nil && strings.TrimSpace(m.UserName) != "" {
				names = append(names, m.UserName)
			}
		}
		if len(names) > 0 {
			b.WriteString(fmt.Sprintf("Members (%d): %s\n", len(names), strings.Join(names, ", ")))
		}
	}

	b.WriteString(fmt.Sprintf("(project_uuid: %s)", proj.Uuid))
	return strings.TrimSpace(b.String()), nil, nil
}
func executeListTeams(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	actingUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	teams, err := teamBusiness.GetDgraphTeamListByUserDgraphUID(ctx, actingUser.Uid)
	if err != nil {
		return "", nil, fmt.Errorf("failed to list teams: %w", err)
	}
	// Narrowed to the asker's teams too, for the same reason as list_projects.
	asker, err := askerView(ctx)
	if err != nil {
		return "", nil, err
	}
	if asker != nil {
		kept := teams[:0:0]
		for _, t := range teams {
			if t != nil && asker.teams[t.Uuid] {
				kept = append(kept, t)
			}
		}
		teams = kept
	}
	if len(teams) == 0 {
		return "You are not a member of any teams.", nil, nil
	}

	adminSet := map[string]bool{}
	if adminTeams, aerr := teamBusiness.GetDgraphTeamListByAdminDgraphUID(ctx, actingUser.Uid); aerr == nil {
		for _, t := range adminTeams {
			if t != nil && t.Uuid != "" {
				adminSet[t.Uuid] = true
			}
		}
	}
	// The list is of the teams they're in; their admin ones are known above.
	makes := func(t *dgraphStruct.DgraphTeam) bool {
		read := &dgraphStruct.DgraphTeam{Uuid: t.Uuid, IsMember: 1, DeletedAt: t.DeletedAt}
		if adminSet[t.Uuid] {
			read.IsAdmin = 1
		}
		return workAccess.CanMakeProjects(ctx, read)
	}

	var b strings.Builder
	b.WriteString("Your teams:\n")
	for _, t := range teams {
		if t == nil || t.Uuid == "" {
			continue
		}
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = "(untitled team)"
		}
		canCreate := "no"
		if makes(t) {
			canCreate = "yes"
		}
		b.WriteString(fmt.Sprintf("- %s [can create projects: %s] (team_uuid: %s)\n", name, canCreate, t.Uuid))
	}

	return strings.TrimSpace(b.String()), nil, nil
}

// executeCreateProject creates a project inside a team. Enforces the same rule
// as controllers/Project/CreateProject: the workspace's "Who can make
// projects" (a team's admins, or everyone in it; Work Access). Write action -
// reaches here only after user confirmation.
func executeCreateProject(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	name := strings.TrimSpace(action.Params["name"])
	teamUUIDStr := strings.TrimSpace(action.Params["team_uuid"])

	if name == "" {
		return "", nil, fmt.Errorf("name is required")
	}
	if teamUUIDStr == "" {
		return "", nil, fmt.Errorf("team_uuid is required")
	}
	teamUUID, err := uuid.Parse(teamUUIDStr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid team UUID")
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	dgraphTeam, err := teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, teamUUIDStr, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTeam == nil || dgraphTeam.Uuid == "" {
		return "", nil, fmt.Errorf("team not found or you don't have access")
	}
	// A DELETED TEAM STILL REPORTS ITS ADMINS. Deleting a team writes a timestamp and
	// leaves its admin edges in place, so the count below answers "yes, an admin" for a
	// team nobody can open — and a project created in it would be immediately orphaned.
	// The lookup did not even select the timestamp until this; it does now.
	if helpers.IsSoftDeleted(dgraphTeam.DeletedAt) {
		return "", nil, fmt.Errorf("team not found or you don't have access")
	}
	if !workAccess.CanMakeProjects(ctx, dgraphTeam) {
		teamName := dgraphTeam.Name
		if teamName == "" {
			teamName = "this team"
		}
		// One of its admins, through an API token, makes them in the app: a token
		// acts as a member (helpers.ActsThroughToken; CanMakeProjects).
		if dgraphTeam.IsAdmin > 0 && helpers.ActsThroughToken(ctx) {
			return "", nil, fmt.Errorf("you can't create projects in %s through a token: its admins make them in the app, and a token acts as a member", teamName)
		}
		return "", nil, fmt.Errorf("you can't create projects in %s: its admins make them", teamName)
	}

	proj, err := projectBusiness.CreateProject(
		ctx,
		name,
		userInfo.UserDgraphInfo.Uid,
		userInfo.UserPostgresInfo.Id,
		dgraphTeam,
		teamUUID,
		&userInfo.UserDgraphInfo,
	)
	if err != nil || proj == nil {
		return "", nil, fmt.Errorf("failed to create project: %w", err)
	}

	teamName := dgraphTeam.Name
	if teamName == "" {
		teamName = "your team"
	}
	return fmt.Sprintf("✅ Project \"%s\" created in %s", name, teamName), nil, nil
}
