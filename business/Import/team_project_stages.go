package business

import (
	"context"
	"fmt"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// runTeamsStage walks every SourceTeam and ensures each has a OneCamp
// team. The team is created with the importing admin as creator+admin
// so the import never produces an unowned team.
//
// If the provider has no teams concept and emits zero entries, we
// still ensure a synthetic "default" team exists per workspace so
// projects always have a parent.
func runTeamsStage(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, importingUser *userModels.UserInfo) error {

	teamCh, errCh := prov.IterTeams(ctx, job, opts)
	got := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				return err
			}
			errCh = nil
		case st, ok := <-teamCh:
			if !ok {
				if got == 0 {
					return ensureDefaultTeam(ctx, job, importingUser)
				}
				return nil
			}
			got++
			if err := resolveOneTeam(ctx, job, &st, importingUser); err != nil {
				importModels.LogImportError(ctx, job.Id, nil,
					importModels.EntityTeam, st.SourceID,
					importModels.SeverityError, "TEAM_CREATE_FAILED",
					err.Error(), nil)
			}
		}
	}
}

// ensureDefaultTeam creates a deterministic per-workspace team and
// records it in the id_map under the synthetic source id "__default__".
func ensureDefaultTeam(ctx context.Context, job *importModels.Job, importingUser *userModels.UserInfo) error {
	const defaultId = "__default__"
	if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityTeam, defaultId); existing != uuid.Nil {
		return nil
	}
	if existing, _ := importModels.LookupWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTeam, defaultId); existing != uuid.Nil {
		_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
			importModels.EntityTeam, defaultId, existing, nil,
			mustMarshal(map[string]any{"matched_by": "workspace_map"}), false)
		return nil
	}

	finalName, err := uniqueTeamName(ctx, teamNameFor(job, ""))
	if err != nil {
		return err
	}
	dgTeam, err := teamBusiness.CreateTeam(ctx, finalName, importingUser)
	if err != nil {
		return fmt.Errorf("CreateTeam %q: %w", finalName, err)
	}
	teamUUID, err := uuid.Parse(dgTeam.Uuid)
	if err != nil {
		return err
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
		importModels.EntityTeam, defaultId, teamUUID, nil,
		mustMarshal(map[string]any{
			"name":       finalName,
			"synthetic":  true,
			"dgraph_uid": dgTeam.Uid,
		}), true); err != nil {
		return err
	}
	_ = importModels.UpsertWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTeam, defaultId, teamUUID, job.Id)
	return nil
}

func resolveOneTeam(ctx context.Context, job *importModels.Job,
	st *importProvider.SourceTeam, importingUser *userModels.UserInfo) error {

	if st.SourceID == "" {
		return nil
	}
	if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityTeam, st.SourceID); existing != uuid.Nil {
		return nil
	}
	if existing, _ := importModels.LookupWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTeam, st.SourceID); existing != uuid.Nil {
		_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
			importModels.EntityTeam, st.SourceID, existing, nil,
			mustMarshal(map[string]any{
				"matched_by": "workspace_map",
				"name":       st.Name,
			}), false)
		addTeamMembers(ctx, job, existing, st)
		return nil
	}

	finalName, err := uniqueTeamName(ctx, teamNameFor(job, st.Name))
	if err != nil {
		return err
	}
	dgTeam, err := teamBusiness.CreateTeam(ctx, finalName, importingUser)
	if err != nil {
		return fmt.Errorf("CreateTeam %q: %w", finalName, err)
	}
	teamUUID, err := uuid.Parse(dgTeam.Uuid)
	if err != nil {
		return err
	}
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
		importModels.EntityTeam, st.SourceID, teamUUID, nil,
		mustMarshal(map[string]any{
			"name":        finalName,
			"source_name": st.Name,
			"description": st.Description,
			"dgraph_uid":  dgTeam.Uid,
		}), true); err != nil {
		return err
	}
	_ = importModels.UpsertWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTeam, st.SourceID, teamUUID, job.Id)

	addTeamMembers(ctx, job, teamUUID, st)
	return nil
}

// addTeamMembers wires team_members + team_admins. Members not yet in
// id_map are silently skipped — re-running the import picks them up.
func addTeamMembers(ctx context.Context, job *importModels.Job, teamUUID uuid.UUID, st *importProvider.SourceTeam) {
	all := append([]string{}, st.MemberIds...)
	all = append(all, st.AdminIds...)
	idMap, err := importDomain.LookupIdMappingsBatch(ctx, job.Id, importModels.EntityUser, all)
	if err != nil {
		helpers.LogWarnWithContext(ctx,
			"Import.addTeamMembers lookup failed team=%s err=%+v", teamUUID, err)
		return
	}
	adminSet := make(map[string]struct{}, len(st.AdminIds))
	for _, id := range st.AdminIds {
		adminSet[id] = struct{}{}
	}
	for _, sid := range st.MemberIds {
		ocUUID, ok := idMap[sid]
		if !ok {
			continue
		}
		dgUid, err := dgraphUidForUser(ctx, ocUUID)
		if err != nil || dgUid == "" {
			continue
		}
		_ = teamBusiness.AddMemberToTeam(ctx, teamUUID, dgUid)
		if _, isAdmin := adminSet[sid]; isAdmin {
			_ = teamBusiness.AddAdminMemberToTeam(ctx, teamUUID, dgUid)
		}
	}
}

// runProjectsStage walks every SourceProject and creates a OneCamp
// project under the right team. Project members are wired and a
// per-project task chunk is enqueued.
//
// Project ordering: projects whose TeamSourceID hasn't been mapped yet
// fall back to the synthetic default team. This makes the stage
// resilient to providers that emit projects before teams.
func runProjectsStage(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, importingUser *userModels.UserInfo) error {

	projCh, errCh := prov.IterProjects(ctx, job, opts)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				return err
			}
			errCh = nil
		case sp, ok := <-projCh:
			if !ok {
				return nil
			}
			if err := resolveOneProject(ctx, job, &sp, importingUser); err != nil {
				importModels.LogImportError(ctx, job.Id, nil,
					importModels.EntityProject, sp.SourceID,
					importModels.SeverityError, "PROJECT_CREATE_FAILED",
					err.Error(), nil)
			}
		}
	}
}

func resolveOneProject(ctx context.Context, job *importModels.Job,
	sp *importProvider.SourceProject, importingUser *userModels.UserInfo) error {

	if sp.SourceID == "" {
		return nil
	}
	if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityProject, sp.SourceID); existing != uuid.Nil {
		return nil
	}
	if existing, _ := importModels.LookupWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityProject, sp.SourceID); existing != uuid.Nil {
		_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
			importModels.EntityProject, sp.SourceID, existing, nil,
			mustMarshal(map[string]any{
				"matched_by": "workspace_map",
				"name":       sp.Name,
			}), false)
		addProjectMembers(ctx, job, existing, sp)
		_ = scheduleProjectTaskChunk(ctx, job.Id, sp.SourceID)
		return nil
	}

	teamSrc := sp.TeamSourceID
	if teamSrc == "" {
		teamSrc = "__default__"
	}
	teamUUID, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityTeam, teamSrc)
	if teamUUID == uuid.Nil {
		teamUUID, _ = importModels.LookupIdMapping(ctx, job.Id, importModels.EntityTeam, "__default__")
		if teamUUID == uuid.Nil {
			if err := ensureDefaultTeam(ctx, job, importingUser); err != nil {
				return fmt.Errorf("ensure default team: %w", err)
			}
			teamUUID, _ = importModels.LookupIdMapping(ctx, job.Id,
				importModels.EntityTeam, "__default__")
		}
	}
	if teamUUID == uuid.Nil {
		return fmt.Errorf("no team available for project %s", sp.SourceID)
	}

	dgTeam, err := teamDomain.GetBasicDgraphTeamInfoByUUID(ctx, teamUUID.String(),
		importingUser.UserDgraphInfo.Uid)
	if err != nil || dgTeam == nil {
		return fmt.Errorf("get team dgraph: %w", err)
	}

	finalName, err := uniqueProjectName(ctx, sp.Name, teamUUID)
	if err != nil {
		return err
	}

	dgProject, err := projectBusiness.CreateProject(ctx, finalName,
		importingUser.UserDgraphInfo.Uid,
		importingUser.UserPostgresInfo.Id,
		dgTeam, teamUUID,
		&importingUser.UserDgraphInfo,
	)
	if err != nil {
		return fmt.Errorf("CreateProject %q: %w", finalName, err)
	}
	projectUUID, err := uuid.Parse(dgProject.Uuid)
	if err != nil {
		return err
	}

	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
		importModels.EntityProject, sp.SourceID, projectUUID, nil,
		mustMarshal(map[string]any{
			"name":        finalName,
			"source_name": sp.Name,
			"description": sp.Description,
			"team_uuid":   teamUUID.String(),
			"team_src_id": sp.TeamSourceID,
			"dgraph_uid":  dgProject.Uid,
		}), true); err != nil {
		return err
	}
	_ = importModels.UpsertWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityProject, sp.SourceID, projectUUID, job.Id)

	addProjectMembers(ctx, job, projectUUID, sp)
	return scheduleProjectTaskChunk(ctx, job.Id, sp.SourceID)
}

// scheduleProjectTaskChunk enqueues exactly one project_tasks chunk per
// project. Idempotent on the unique chunk index.
func scheduleProjectTaskChunk(ctx context.Context, importId uuid.UUID, projectSrcId string) error {
	return importModels.CreateChunks(ctx, []*importModels.Chunk{{
		Id:             uuid.New(),
		ImportId:       importId,
		ChunkType:      importModels.ChunkProjectTasks,
		ParentSourceId: strPtr(projectSrcId),
		Status:         importModels.ChunkStatusPending,
		MaxAttempts:    5,
	}})
}

func addProjectMembers(ctx context.Context, job *importModels.Job,
	projectUUID uuid.UUID, sp *importProvider.SourceProject) {

	all := append([]string{}, sp.MemberIds...)
	all = append(all, sp.AdminIds...)
	idMap, err := importDomain.LookupIdMappingsBatch(ctx, job.Id, importModels.EntityUser, all)
	if err != nil {
		return
	}
	adminSet := make(map[string]struct{}, len(sp.AdminIds))
	for _, id := range sp.AdminIds {
		adminSet[id] = struct{}{}
	}
	for _, sid := range sp.MemberIds {
		ocUUID, ok := idMap[sid]
		if !ok {
			continue
		}
		dgUid, err := dgraphUidForUser(ctx, ocUUID)
		if err != nil || dgUid == "" {
			continue
		}
		_ = projectBusiness.AddMemberToProject(ctx, projectUUID, dgUid, ocUUID.String())
		if _, isAdmin := adminSet[sid]; isAdmin {
			_ = projectBusiness.AddAdminMemberToProject(ctx, projectUUID, dgUid)
		}
	}
}

// teamNameFor builds a human-readable team name with a provider tag so
// re-imports of multiple workspaces don't collide.
func teamNameFor(job *importModels.Job, sourceName string) string {
	if sourceName == "" {
		return strings.TrimSpace(job.SourceWorkspaceName + " (from " + job.Provider + ")")
	}
	return strings.TrimSpace(sourceName + " (from " + job.Provider + ")")
}

// uniqueTeamName resolves collisions by suffixing -2, -3 …
func uniqueTeamName(ctx context.Context, desired string) (string, error) {
	if exists, _ := teamBusiness.CheckIfTeamExistByTeamName(ctx, desired); !exists {
		return desired, nil
	}
	for i := 2; i < 1000; i++ {
		c := fmt.Sprintf("%s-%d", desired, i)
		if exists, _ := teamBusiness.CheckIfTeamExistByTeamName(ctx, c); !exists {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find a free team name from base %q", desired)
}

// uniqueProjectName resolves collisions within the given team.
func uniqueProjectName(ctx context.Context, desired string, teamUUID uuid.UUID) (string, error) {
	desired = strings.TrimSpace(desired)
	if desired == "" {
		desired = "Imported project"
	}
	exists, err := projectDomain.CheckIfProjectExistByProjectNameAndTeamID(ctx, desired, teamUUID)
	if err == nil && !exists {
		return desired, nil
	}
	for i := 2; i < 1000; i++ {
		c := fmt.Sprintf("%s-%d", desired, i)
		exists, err := projectDomain.CheckIfProjectExistByProjectNameAndTeamID(ctx, c, teamUUID)
		if err == nil && !exists {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find a free project name from base %q", desired)
}

// dgraphUidForUser caches a single oc-uuid → dgraph-uid lookup.
func dgraphUidForUser(ctx context.Context, ocUUID uuid.UUID) (string, error) {
	dgUser, err := getDgraphUserDirect(ctx, ocUUID)
	if err != nil || dgUser == nil {
		return "", err
	}
	return dgUser.Uid, nil
}
