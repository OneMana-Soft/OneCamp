package business

import (
	"context"
	"errors"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Project"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	userProjectNotificationBusiness "github.com/akashc777/OneCamp/business/UserProjectNotification"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func CreateProject(ctx context.Context, projectName string, createdByUserDgraphUID string, createdByUserUUID uuid.UUID, teamDgraphInfo *dgraphStruct.DgraphTeam, teamUUID uuid.UUID, userDgraphInfo *dgraphStruct.DgraphUser) (dgraphProject *dgraphStruct.DgraphProject, err error) {

	currentTime := time.Now()
	projectUUID := uuid.New()
	zeroUnixTime := time.Time{}

	err = domain.CreateProject(ctx, projectUUID, projectName, teamUUID, createdByUserUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateProject failed to create new project in postgres err: %+v", err)
		return
	}

	dgraphProject = &dgraphStruct.DgraphProject{
		Uid:  "uid(project)",
		Uuid: projectUUID.String(),
		Members: []*dgraphStruct.DgraphUser{{
			Uid: createdByUserDgraphUID,
			Projects: []*dgraphStruct.DgraphProject{{
				Uid: "uid(project)",
			}},
		}},
		Admins: []*dgraphStruct.DgraphUser{{
			Uid: createdByUserDgraphUID,
		}},
		Team: &dgraphStruct.DgraphTeam{
			Uid: teamDgraphInfo.Uid,
			Projects: []*dgraphStruct.DgraphProject{{
				Uid: "uid(project)",
			}},
		},
		Name:      projectName,
		DeletedAt: &zeroUnixTime,
		CreatedAt: &currentTime,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: userDgraphInfo.Uid,
		},
	}

	projectUID, err := domain.CreateOrUpdateDgraphProject(ctx, dgraphProject)

	// This read `len(projectUID) == 0 && err != nil` — AND, so it only caught the case where BOTH
	// signals were bad. A Dgraph error that still returned a uid, and an empty uid with no error,
	// both walked straight past it, and the function carried on to create notification rows and
	// an OpenSearch document for a project that may not exist. Either signal alone means the
	// project was not created, which is what the shared rule says.
	if helpers.DgraphWriteFailed(projectUID, err) {
		helpers.LogErrorWithContext(ctx, "business/CreateProject failed to create new project in dgraph err: %+v", err)

		// An empty uid with a nil error is still a failure; name it rather than returning nil.
		if err == nil {
			err = errors.New("dgraph returned no uid for the new project")
		}

		// Reverse the Postgres insert. Leaving it made this PERMANENTLY UNREPEATABLE: projects.
		// project_name is UNIQUE and project listings read Dgraph, so the row was a project
		// nobody could see that still owned the name, and every retry failed on the constraint.
		//
		// Safe here because tasks, notifications and github links are all written later, and they
		// are the only things referencing projects(id).
		_ = helpers.CompensateOnFailure(ctx, "project row for "+projectName,
			func(undoCtx context.Context) error {
				return domain.HardDeleteProject(undoCtx, projectUUID)
			})

		return
	}

	go userProjectNotificationBusiness.CreateProjectNotificationType(userDgraphInfo.Uuid, projectUUID.String(), postgressStruct.NOTIFICATION_TYPE_ALL)

	opensearchProject := &openSearchStruct.OpenSearchProject{
		Uuid:             projectUUID.String(),
		ProjectName:      projectName,
		ProjectTeamUuid:  teamDgraphInfo.Uuid,
		ProjectTeamName:  teamDgraphInfo.Name,
		ProjectCreatedAt: currentTime.Unix(),
		ProjectDeletedAt: nil,
	}

	go domain.CreateProjectInOpenSearch(opensearchProject)

	return
}

func UpdateProjectName(ctx context.Context, projectName string, projectUUID uuid.UUID) (err error) {
	currentTime := time.Now()

	err = domain.UpdateProjectNameByProjectUUID(ctx, projectName, projectUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateProjectName failed to update project name in postgres err: %+v", err)
		return
	}

	dgraphProject := dgraphStruct.DgraphProject{
		Uid:       "uid(project)",
		Uuid:      projectUUID.String(),
		Name:      projectName,
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, &dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateProjectName failed to update project name in dgraph err: %+v", err)
		return
	}

	opensearchProject := &openSearchStruct.OpenSearchProject{
		Uuid:             projectUUID.String(),
		ProjectName:      projectName,
		ProjectUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateProjectInOpenSearch(opensearchProject)

	return
}

func AddMemberToProject(ctx context.Context, projectUUID uuid.UUID, userDgraphUID string, userDgraphUUID string) (err error) {

	dgraphProject := dgraphStruct.DgraphProject{
		Uid:  "uid(project)",
		Uuid: projectUUID.String(),
		Members: []*dgraphStruct.DgraphUser{{
			Uid: userDgraphUID,
			Projects: []*dgraphStruct.DgraphProject{{
				Uid: "uid(project)",
			}},
		}},
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, &dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddMemberToProject failed to add member to project in dgraph err: %+v", err)
		return
	}

	go userProjectNotificationBusiness.CreateProjectNotificationType(userDgraphUUID, projectUUID.String(), postgressStruct.NOTIFICATION_TYPE_ALL)

	return
}

func AddAdminMemberToProject(ctx context.Context, projectUUID uuid.UUID, userDgraphUID string) (err error) {

	dgraphProject := dgraphStruct.DgraphProject{
		Uid:  "uid(project)",
		Uuid: projectUUID.String(),
		Admins: []*dgraphStruct.DgraphUser{{
			Uid: userDgraphUID,
		}},
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, &dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAdminMemberToProject failed to add admin member to project in dgraph err: %+v", err)
		return
	}

	return
}

func RemoveMemberFromProject(ctx context.Context, userDgraphUID string, userDrgaphUUID string, projectDgraphUID string, projectDgraphUUID string) (err error) {

	err = domain.RemoveMemberFromProject(ctx, projectDgraphUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveMemberFromProject failed to remove admin member from project in dgraph err: %+v", err)
		return
	}

	go userProjectNotificationBusiness.DeleteNotificationTypeWhenUserIsRemovedFormProject(userDrgaphUUID, projectDgraphUUID)

	return
}

func RemoveAdminMemberFromProject(ctx context.Context, userDgraphUID string, projectDgraphUID string) (err error) {

	err = domain.RemoveAdminMemberFromProject(ctx, projectDgraphUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAdminMemberFromProject failed to remove admin member from project in dgraph err: %+v", err)
		return
	}

	return
}

func ArchiveProjectByProjectUUID(ctx context.Context, projectUUID uuid.UUID, dgraphProjectInfo *dgraphStruct.DgraphProject) (err error) {

	currentTime := time.Now()

	err = domain.UpdateProjectDeletedTimeByUUID(ctx, projectUUID, &currentTime, &currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveProjectByProjectUUID failed to update delete time err: %+v", err)
		return
	}

	dgraphProject := &dgraphStruct.DgraphProject{
		Uid:       "uid(project)",
		Uuid:      projectUUID.String(),
		DeletedAt: &currentTime,
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveProjectByProjectUUID failed to update delete time in postgres err: %+v", err)
		return
	}

	opensearchProject := &openSearchStruct.OpenSearchProject{
		Uuid:             projectUUID.String(),
		ProjectDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}
	go domain.UpdateProjectWithAttachmentsInOpenSearch(opensearchProject, dgraphProjectInfo)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"task_project_id", "comment_project_id", "attachment_project_id"},
		projectUUID.String(),
		currentTime.Unix(),
		[]string{"tasks", "comments", "attachments"},
		"cascade",
	)
	// Cascade soft-delete to AI embeddings (tasks + comments in this project)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"project_uuid"},
		projectUUID.String(),
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	// Cascade the archive into the memory layer by SCOPE so facts derived
	// from this project's tasks/threads stop surfacing. Reversible via
	// UnArchiveProjectUUID. Own goroutine so a slow memory store never
	// extends the archive request.
	go func(prUUID string) {
		defer recoverProjectMemoryCascade("archive")
		ai.ArchiveMemoryByScope(context.Background(), memoryModels.ScopeRef{ProjectUUID: prUUID})
	}(projectUUID.String())

	return

}

func UnArchiveProjectUUID(ctx context.Context, projectUUID uuid.UUID, dgraphProjectInfo *dgraphStruct.DgraphProject) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	err = domain.UpdateProjectDeletedTimeToNullByUUID(ctx, projectUUID, &currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveProjectUUID failed to update delete time in postgres err: %+v", err)
		return
	}

	dgraphProject := &dgraphStruct.DgraphProject{
		Uid:       "uid(project)",
		Uuid:      projectUUID.String(),
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveProjectUUID failed to update delete time in dgraph err: %+v", err)
		return
	}

	opensearchProject := &openSearchStruct.OpenSearchProject{
		Uuid:             projectUUID.String(),
		ProjectDeletedAt: nil,
	}

	go domain.UpdateProjectWithAttachmentsInOpenSearch(opensearchProject, dgraphProjectInfo)
	go globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
		[]string{"task_project_id", "comment_project_id", "attachment_project_id"},
		projectUUID.String(),
		[]string{"tasks", "comments", "attachments"},
		"cascade",
	)
	// Cascade unarchive to AI embeddings
	go globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
		[]string{"project_uuid"},
		projectUUID.String(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	// Revive the memory items the archive cascade removed for this project
	// (never user-deleted ones). Mirrors the OpenSearch unarchive above.
	go func(prUUID string) {
		defer recoverProjectMemoryCascade("restore")
		ai.RestoreMemoryByScope(context.Background(), memoryModels.ScopeRef{ProjectUUID: prUUID})
	}(projectUUID.String())

	return
}

// recoverProjectMemoryCascade guards a fire-and-forget project memory
// cascade goroutine so a panic in the best-effort AI projection layer can
// never crash the server. phase is "archive" or "restore" for the log.
func recoverProjectMemoryCascade(phase string) {
	if r := recover(); r != nil {
		helpers.MessageLogs.ErrorLog.Printf("panic in project memory %s cascade: %v", phase, r)
	}
}

func GetBasicDgraphProjectInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetBasicDgraphProjectInfo(ctx, projectUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphProjectInfo failed to get project from dgraph err: %+v", err)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithTaskUUID(ctx context.Context, projectUUID string, userDgraphUID string, taskUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetBasicDgraphProjectInfoWithTaskUUID(ctx, projectUUID, userDgraphUID, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphProjectInfoWithTaskUUID failed to get project from dgraph err: %+v", err)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx context.Context, projectUUID string, userDgraphUID string, memberUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {

	dgraphProject, err = domain.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, projectUUID, userDgraphUID, memberUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphProjectInfoWithGivenMemberUUID failed to get project info from dgraph err: %+v", err)
		return
	}

	return

}

func GetDgraphProjectCombinedMemberInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectMemberInfo(ctx, projectUUID, userDgraphUID)

	if err != nil || dgraphProject == nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectCombinedMemberInfo failed to get project from dgraph err: %+v", err)
		return
	}

	adminIndex := 0
	adminLength := len(dgraphProject.Admins)

	if adminLength > 0 {

		for _, member := range dgraphProject.Members {
			if member.Uuid == dgraphProject.Admins[adminIndex].Uuid {
				member.IsAdmin = true
				adminIndex = adminIndex + 1
			}

			if adminIndex == adminLength {
				break
			}
		}

	}

	// dgraphProject.Admins = nil

	return

}

func GetDgraphProjectMemberInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectMemberInfo(ctx, projectUUID, userDgraphUID)

	if err != nil || dgraphProject == nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectMemberInfo failed to get project from dgraph err: %+v", err)
		return
	}

	adminIndex := 0
	adminLength := len(dgraphProject.Admins)

	if adminLength > 0 {

		for _, member := range dgraphProject.Members {
			if member.Uuid == dgraphProject.Admins[adminIndex].Uuid {
				member.IsAdmin = true
				adminIndex = adminIndex + 1
			}

			if adminIndex == adminLength {
				break
			}
		}

	}

	dgraphProject.Admins = nil

	return
}

func GetDgraphProjectListByAdminDgraphUID(ctx context.Context, userDgraphUID string) (dgraphProject []*dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectListByAdminDgraphUID(ctx, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectListByAdminDgraphUID failed to get project list from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphProjectTaskList(ctx context.Context, projectUUID string, userDgraphUID string, filterQuery string, sortQuery string, pageSize int, pageIndex int, getAll bool) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectTaskList(ctx, projectUUID, userDgraphUID, filterQuery, sortQuery, pageSize, pageIndex, getAll)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectTaskList failed to get project task list from dgraph err: %+v", err)
		return
	}

	if dgraphProject != nil {
		_ = taskBusiness.MergeGitHubMetaIntoTasks(ctx, dgraphProject.Tasks)
	}

	return
}

func GetDgraphProjectTaskListForKanban(ctx context.Context, projectUUID string, userDgraphUID string, filterQuery string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectTaskListForKanban(ctx, projectUUID, userDgraphUID, filterQuery)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectTaskListForKanban failed to get project task list from dgraph err: %+v", err)
		return
	}

	if dgraphProject != nil {
		for _, column := range [][]*dgraphStruct.DgraphTask{dgraphProject.TasksTodo, dgraphProject.TasksInProgresss, dgraphProject.TasksBacklog, dgraphProject.TasksInReview, dgraphProject.TasksCanceled, dgraphProject.TasksDone} {
			taskrank.Sort(column)
		}
		allTasks := append([]*dgraphStruct.DgraphTask{}, dgraphProject.TasksTodo...)
		allTasks = append(allTasks, dgraphProject.TasksInProgresss...)
		allTasks = append(allTasks, dgraphProject.TasksBacklog...)
		allTasks = append(allTasks, dgraphProject.TasksInReview...)
		allTasks = append(allTasks, dgraphProject.TasksCanceled...)
		allTasks = append(allTasks, dgraphProject.TasksDone...)
		_ = taskBusiness.MergeGitHubMetaIntoTasks(ctx, allTasks)
	}

	return
}

func GetDgraphProjectAttachmentsInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectAttachmentsInfo(ctx, projectUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectAttachmentsInfo failed to get project task list from dgraph err: %+v", err)
		return
	}

	return
}

func AddAttachmentToProjectDgraph(ctx context.Context, projectUUID uuid.UUID, projectInput *adapter.CreateOrUpdateProjectInput, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	err = domain.UpdateProjectDeletedTimeToNullByUUID(ctx, projectUUID, &currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAttachmentToProjectDgraph failed to update project in postgres err: %+v", err)
		return
	}

	for _, mediaObj := range projectInput.Attachments {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphProject := &dgraphStruct.DgraphProject{
		Uid:         "uid(project)",
		Uuid:        projectUUID.String(),
		Attachments: projectInput.Attachments,
	}

	_, err = domain.CreateOrUpdateDgraphProject(ctx, dgraphProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAttachmentToProjectDgraph failed to add attachments to project err: %+v", err)
		return
	}

	go domain.CreateProjectkAttachmentsInOpensearch(&currentTime, projectUUID.String(), projectInput.Attachments, userInfo.Uuid)

	return

}

func RemoveAttachmentFromProject(ctx context.Context, attachmentUUID uuid.UUID) (err error) {
	err = attachmentBusiness.ArchiveAttachmentByAttachmentUUID(ctx, attachmentUUID, "", "")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAttachmentFromProject failed to delete attachment from project err: %+v", err)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID(ctx context.Context, projectUUID string, userDgraphUID string, memberUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID(ctx, projectUUID, userDgraphUID, memberUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID failed to get projectInfo err: %+v", err)
		return
	}

	return
}

func GetDgraphProjectInfoAndTeamAdminFlag(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	dgraphProject, err = domain.GetDgraphProjectInfoAndTeamAdminFlagAndAttachments(ctx, projectUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphProjectInfoAndTeamAdminFlag failed to get projectInfo err: %+v", err)
		return
	}

	return
}
