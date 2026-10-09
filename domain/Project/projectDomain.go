package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Project"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Project"
	models "github.com/akashc777/OneCamp/models/postgres/Project"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const projectTaskFields = `
						id: task_uuid
						task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_github_issue_number
						task_github_issue_url
						task_github_pr_number
						task_github_pr_url
						task_github_branch
						task_github_pr_state
						task_github_pr_check_status
						task_github_pr_review_state
						task_github_pr_is_draft
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
						` + dgraphStruct.TASK_BLOCKED_OPEN

func CreateProject(ctx context.Context, projectUUID uuid.UUID, projectName string, teamUUID uuid.UUID, createdByUUID uuid.UUID, createdTime time.Time) (err error) {

	query := `
		INSERT INTO projects (id, project_name, team_id, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
	`
	err = models.CreateProject(query, projectUUID, projectName, teamUUID, createdByUUID, createdTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProject Failed to create new project err: %+v",
			err)
		return
	}
	return
}

func CheckIfProjectExistByProjectNameAndTeamID(ctx context.Context, projectName string, teamUUID uuid.UUID) (exist bool, err error) {
	query := `
        SELECT EXISTS (
            SELECT 1
            FROM projects
            WHERE project_name = $1 AND team_id = $2
        );
    `

	exist, err = models.CheckIfProjectExistByProjectNameAndTeamUUID(query, projectName, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfProjectExistByProjectNameAndTeamID Failed to check project by project name and teamID err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectNameByProjectUUID(ctx context.Context, newProjectName string, projectUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
        UPDATE projects
        SET project_name = $1, updated_at = $2
		WHERE id = $3`

	err = models.UpdateProjectNameByProjectUUID(query, newProjectName, currentTime, projectUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamNameByTeamUUID Failed to update project name by project uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectDeletedTimeByUUID(ctx context.Context, projectUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	query := `
        UPDATE projects
        SET deleted_at = $1, updated_at = $2
		WHERE id = $3`
	err = models.UpdateProjectDeletedTimeByUUID(query, projectUUID, deleteTime, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectDeletedTimeByUUID Failed to update project delete time by project uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateProjectDeletedTimeToNullByUUID(ctx context.Context, projectUUID uuid.UUID, updatedTime *time.Time) (err error) {
	query := `
        UPDATE projects
        SET updated_at = $1, deleted_at = null
		WHERE id = $2`
	err = models.UpdateProjectDeletedTimeToNullByUUID(query, projectUUID, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectDeletedTimeToNullByUUID Failed to update delete time to null err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphProject(ctx context.Context, dgraphProject *dgraphStruct.DgraphProject) (projectUid string, err error) {
	dgraphProject.DType = []string{"Project"}
	dgraphProject.Uid = "uid(project)"
	query := fmt.Sprintf(`query {
									  project as var(func: eq(project_uuid, "%+v"))
								  }`, dgraphProject.Uuid)

	projectUid, err = dgraphModels.CreateOrUpdateDgraphProject(ctx, dgraphProject, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"Domain/CreateOrUpdateDgraphProject Failed to create project in dgraph err: %+v",
			err,
		)
		return
	}

	// Invalidate cache
	if dgraphProject.Uuid != "" {
		_ = redisStore.DeletePattern(ctx, registry.ProjectTasks.Pattern(dgraphProject.Uuid))
	}

	return
}

// GetDgraphProjectListByAdminDgraphUID is the live projects a person admins:
// the ones work can be put in. Archived projects are left out, so pickers
// (new task, board to tasks, agents, workflows) never offer them.
func GetDgraphProjectListByAdminDgraphUID(ctx context.Context, userDgraphUID string) (dgraphProject []*dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$user_id"] = userDgraphUID
	query := `query ProjectInfo($user_id: string){
				projectInfo(func: has(project_uuid)) @filter(uid_in(project_admins, $user_id) AND NOT gt(project_deleted_at, "1970-01-01T00:00:00Z")) {
					uid
					project_uuid
					project_name
					project_status
					project_team {
						uid
						team_uuid
						team_name
					}
					project_members @filter(NOT eq(is_external, true)) {
						user_uuid
						user_name
						user_profile_object_key
					}

				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectInfoByUUID Failed to get project list from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphProjectTaskListForKanban(ctx context.Context, projectUUID string, userDgraphUID string, filterQuery string, closedLimit int) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	if len(filterQuery) > 0 {
		filterQuery = "AND " + filterQuery
	}
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userDgraphUID"] = userDgraphUID

	query := fmt.Sprintf(`query ProjectInfo($id: string, $userDgraphUID: string){
			projectInfo(func: eq(project_uuid, $id)) {
				project_uuid
				project_name
				project_is_member: count(project_members @filter(uid($userDgraphUID)))
				project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
				project_members @filter(NOT eq(is_external, true)) (orderasc: user_name){
					uid
					user_uuid
					user_name
					user_profile_object_key
				}
				project_tasks_todo: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "todo") %s) (orderdesc: task_created_at) {
					%s
				}
				project_tasks_in_progress: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "inProgress") %s) (orderdesc: task_created_at) {
					%s
				}
				project_tasks_backlog: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "backlog") %s) (orderdesc: task_created_at) {
					%s
				}
				project_tasks_in_review: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "inReview") %s) (orderdesc: task_created_at) {
					%s
				}
				project_tasks_canceled: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "canceled") %s) (orderdesc: task_created_at%s) {
					%s
				}
				project_tasks_canceled_count: count(project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "canceled") %s))
				project_tasks_done: project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "done") %s) (orderdesc: task_created_at%s) {
					%s
				}
				project_tasks_done_count: count(project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND eq(task_status, "done") %s))
			}
		}`, filterQuery, projectTaskFields, filterQuery, projectTaskFields, filterQuery, projectTaskFields, filterQuery, projectTaskFields,
		filterQuery, dgraphStruct.ClosedFirst(closedLimit), projectTaskFields, filterQuery,
		filterQuery, dgraphStruct.ClosedFirst(closedLimit), projectTaskFields, filterQuery)

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"Domain/GetDgraphProjectTaskListForKanban Failed to get project task list in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphProjectTaskList(ctx context.Context, projectUUID string, userDgraphUID string, filterQuery string, sortQuery string, pageSize int, pageIndex int, getAll bool) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	offset := pageIndex * pageSize

	if len(filterQuery) > 0 {
		filterQuery = "AND " + filterQuery
	}
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userDgraphUID"] = userDgraphUID
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	if !getAll {
		sortQuery = fmt.Sprintf("first: %v, offset: %v, ", firstVal, offsetVal) + sortQuery
	}

	query := fmt.Sprintf(`query ProjectInfo($id: string, $userDgraphUID: string){
				projectInfo(func: eq(project_uuid, $id)) {
					project_uuid
					project_name
					project_is_member: count(project_members @filter(uid($userDgraphUID)))
					project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
					project_task_count: count(project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") %s))
					project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") %s) ( %s) {
						task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_github_issue_number
						task_github_issue_url
						task_github_pr_number
						task_github_pr_url
						task_github_branch
						task_github_pr_state
						task_github_pr_check_status
						task_github_pr_review_state
						task_github_pr_is_draft
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
					}
				}
			}`, filterQuery, filterQuery, sortQuery)

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectTaskList Failed to get project task list from dgraph err: %+v",
			err,
		)
		return
	}
	return

}

func GetDgraphProjectInfoAndTeamAdminFlagAndAttachments(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					project_uuid
					project_created_at
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_team {
						uid
						team_uuid
						team_is_admin: count(team_admins @filter(uid($userUid)))
					}
					project_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")) {
						attachment_uuid
					}
					project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) {
						task_uuid
						task_attachments {
							attachment_uuid
						}
						task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
							comment_uuid
							comment_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")) {
								attachment_uuid
							}
						}
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectInfoAndTeamAdminFlag Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphProjectMemberInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					project_uuid
					project_name
					project_is_member: count(project_members @filter(uid($userUid)))
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_admins (orderasc: user_name){
						user_uuid
						
					}
					project_members @filter(NOT eq(is_external, true)) (orderasc: user_name){
						uid
						user_uuid
						user_name
						user_email
						user_profile_object_key
					}
					project_created_by {
						user_uuid
					}
					project_team {
						team_is_admin: count(team_admins @filter(uid($userUid)))
						
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectMemberInfo Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphProjectAttachmentsInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {

	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					project_uuid
				
					project_is_member: count(project_members @filter(uid($userUid)))
					project_is_admin: count(project_admins @filter(uid($userUid)))
					
					project_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")){
						attachment_uuid
						attachment_file_name
						attachment_obj_key
						attachment_width
						attachment_height
						attachment_size
						attachment_raw_type
						attachment_type
						attachment_duration
						attachment_created_at
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphProjectInfo Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID(ctx context.Context, projectUUID string, userDgraphUID string, memberUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	variables["$memberUuid"] = memberUUID
	query := `query ProjectInfo($id: string, $userUid: string, $memberUuid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					uid
					project_uuid
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_created_by {
						uid
						user_uuid
					}
					project_team {
						team_members @filter(eq(user_uuid, $memberUuid)){
							uid
							user_uuid
						}
						team_is_admin: count(team_admins @filter(uid($userUid)))
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx context.Context, projectUUID string, userDgraphUID string, memberUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	variables["$memberUuid"] = memberUUID
	query := `query ProjectInfo($id: string, $userUid: string, $memberUuid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					uid
					project_uuid
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_members @filter(eq(user_uuid, $memberUuid)){
						uid
						user_uuid
						user_name
					}
					project_created_by {
						uid
						user_uuid
					}
					project_team {
						team_is_admin: count(team_admins @filter(uid($userUid)))
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphProjectInfoWithGivenMemberUUID to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicDgraphProjectInfoWithTaskUUID(ctx context.Context, projectUUID string, userDgraphUID string, taskUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	variables["$taskUid"] = taskUUID
	query := `query ProjectInfo($id: string, $userUid: string, $taskUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					uid
					project_uuid
					project_name
					project_status
					project_is_member: count(project_members @filter(uid($userUid)))
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_team {
						uid
						team_uuid
						team_name
					}
					project_tasks @filter(eq(task_uuid, $taskUid)){
						uid
						task_uuid
					}
					project_created_by {
						user_uuid
						user_name
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphProjectInfoWithTaskUUID Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicDgraphProjectInfo(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					uid
					project_uuid
					project_name
					project_status
					project_deleted_at
					project_is_member: count(project_members @filter(uid($userUid)))
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_team {
						uid
						team_uuid
						team_name
					}
					project_created_by {
						user_uuid
						user_name
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphProjectInfo Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func RemoveAdminMemberFromProject(ctx context.Context, projectDgraphUID string, userDgraphUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		
			{
				"uid": "%s",
				"project_admins": [
					{
						"uid": "%s"
					}
				]
			}
		
	`, projectDgraphUID, userDgraphUID)

	err = dgraphModels.DeleteProjectEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/RemoveAdminMemberFromProject Failed to remove admin in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func RemoveMemberFromProject(ctx context.Context, projectDgraphUID string, userDgraphUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			{
				"uid": "%s",
				"project_members": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"project_admins": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"user_projects": [
					{
						"uid": "%s"
					}
				]
			}
		]
	`, projectDgraphUID, userDgraphUID, projectDgraphUID, userDgraphUID, userDgraphUID, projectDgraphUID)

	err = dgraphModels.DeleteProjectEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteProjectMemberEdge Failed to remove tproject member in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateProjectInOpenSearch(openSearchProject *openSearchStruct.OpenSearchProject) {
	ctx := context.Background()
	err := OpenSearchModels.CreateProjectInOpenSearch(ctx, openSearchProject)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectInOpenSearch Failed to create project in opensearch err: %+v",
			err)
		return
	}
	return
}

func UpdateProjectInOpenSearch(openSearchProject *openSearchStruct.OpenSearchProject) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateProjectInOpenSearch(ctx, openSearchProject)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectInOpenSearch Failed to update project in opensearch err: %+v",
			err)
		return
	}
	return
}

func getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchProject *openSearchStruct.OpenSearchProject, openSearchAttachments []*openSearchStruct.OpenSearchAttachment, openSearchTasks []*openSearchStruct.OpenSearchTask, openSearchComments []*openSearchStruct.OpenSearchComment) (bulkActionString string, err error) {

	projectUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"%+v\", \"_id\": \"%+v\" } }\n", openSearchStruct.PROJECT_INDEX, openSearchProject.Uuid)
	projectUpdateDoc := &openSearchStruct.BulkUpdate{
		Doc: openSearchProject,
	}
	taskJsonData, err := json.Marshal(projectUpdateDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch Error mashiling project struct to json err: %+v",
			err)
		return
	}

	projectUpdate += string(taskJsonData) + "\n"

	bulkActionString += projectUpdate

	attachmentUpdate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentUpdate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.ATTACHMENT_INDEX, attachment.Uuid)
		attachmentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: attachment,
		}
		attachmentJsonData, _ := json.Marshal(attachmentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch Error marshiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentUpdate += string(attachmentJsonData) + "\n"
		attachmentUpdate += tempAttachmentUpdate
	}

	bulkActionString += attachmentUpdate

	taskUpdate := ""

	for _, task := range openSearchTasks {
		tempTaskUpdate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.TASK_INDEX, task.Uuid)
		taskUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: task,
		}
		taskJsonData, _ := json.Marshal(taskUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch Error marshiling task struct to json err: %+v",
				err)
			return
		}

		tempTaskUpdate += string(taskJsonData) + "\n"
		taskUpdate += tempTaskUpdate
	}

	bulkActionString += taskUpdate

	commentUpdate := ""

	for _, comment := range openSearchComments {
		tempCommentUpdate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.COMMENT_INDEX, comment.Uuid)
		commentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: comment,
		}
		commentJsonData, _ := json.Marshal(commentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch Error marshiling comment struct to json err: %+v",
				err)
			return
		}

		tempCommentUpdate += string(commentJsonData) + "\n"
		commentUpdate += tempCommentUpdate
	}

	bulkActionString += commentUpdate

	return
}

func UpdateProjectWithAttachmentsInOpenSearch(openSearchProject *openSearchStruct.OpenSearchProject, dgraphProject *dgraphStruct.DgraphProject) {
	ctx := context.Background()
	var openSearchUpdateAttachmentDocs []*openSearchStruct.OpenSearchAttachment
	var openSearchUpdateTaskDocs []*openSearchStruct.OpenSearchTask
	var openSearchUpdateCommentDocs []*openSearchStruct.OpenSearchComment

	for _, attachment := range dgraphProject.Attachments {
		openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
			Uuid:                attachment.Uuid,
			AttachmentDeletedAt: openSearchProject.ProjectDeletedAt,
		})
	}

	for _, task := range dgraphProject.Tasks {
		openSearchUpdateTaskDocs = append(openSearchUpdateTaskDocs, &openSearchStruct.OpenSearchTask{
			Uuid:          task.Uuid,
			TaskDeletedAt: openSearchProject.ProjectDeletedAt,
		})

		for _, attachment := range task.Attachments {
			openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
				Uuid:                attachment.Uuid,
				AttachmentDeletedAt: openSearchProject.ProjectDeletedAt,
			})
		}

		for _, comment := range task.Comments {
			openSearchUpdateCommentDocs = append(openSearchUpdateCommentDocs, &openSearchStruct.OpenSearchComment{
				Uuid:             comment.Uuid,
				CommentDeletedAt: openSearchProject.ProjectDeletedAt,
			})
			for _, attachment := range comment.Attachments {
				openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
					Uuid:                attachment.Uuid,
					AttachmentDeletedAt: openSearchProject.ProjectDeletedAt,
				})
			}

		}
	}

	bulkOperationString, err := getUpdateProjectWithTaskCommentAndAttachmentBulkOperationStringForOpenSearch(ctx, openSearchProject, openSearchUpdateAttachmentDocs, openSearchUpdateTaskDocs, openSearchUpdateCommentDocs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectWithAttachmentsInOpenSearch Error getting bulk string for project's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateProjectWithAttachmentsInOpenSearch Error getting bulk string for project with attachments err: %+v",
			err)
		return
	}

}

func CreateProjectkAttachmentsInOpensearch(currentTime *time.Time, projectUUID string, dgraphAttachments []*dgraphStruct.DgraphAttachment, userUUID string) {

	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachments {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                  attachment.Uuid,
			AttachmentFileName:    attachment.FileName,
			AttachmentByUserUuid:  userUUID,
			AttachmentObjKey:      attachment.ObjectKey,
			AttachmentProjectUuid: projectUUID,
			AttachmentCreatedAt:   currentTime.Unix(),
		})
	}

	bulkOperationString, err := getProjectWithAttachmentBulkOperationStringForOpenSearch(ctx, nil, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectkAttachmentsInOpensearch Error getting bulk string for project's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectkAttachmentsInOpensearch Error getting bulk string for project with attachments err: %+v",
			err)
		return
	}

	return

}

func getProjectWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchProject *openSearchStruct.OpenSearchProject, openSearchAttachments []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	if openSearchProject != nil {
		postCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.PROJECT_INDEX, openSearchProject.Uuid)
		var postJsonData []byte
		postJsonData, err = json.Marshal(openSearchProject)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"domain/getProjectWithAttachmentBulkOperationStringForOpenSearch Error mashiling project struct to json err: %+v",
				err)
			return
		}

		postCreate += string(postJsonData)

		bulkActionString += postCreate + "\n"
	}

	attachmentCreate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.ATTACHMENT_INDEX, attachment.Uuid)
		attachmentJsonData, _ := json.Marshal(attachment)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getProjectWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

// HardDeleteProject removes a project row outright.
//
// Compensation only: see models.HardDeleteProject for why a soft delete cannot serve here
// (project_name is UNIQUE and a soft-deleted row keeps the name).
func HardDeleteProject(ctx context.Context, projectUUID uuid.UUID) (err error) {
	query := `DELETE FROM projects WHERE id = $1`
	err = models.HardDeleteProject(query, projectUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteProject Failed to delete project row err: %+v", err)
		return
	}
	return
}

func GetProjectByUUID(ctx context.Context, TeamUUID uuid.UUID) (projectInfo *models.Project, err error) {

	query := `
        SELECT id, project_name, created_by, team_id, created_at, updated_at, deleted_at
        FROM projects
        WHERE id = $1
    `

	projectInfo, err = models.GetProjectByUUID(query, TeamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetProjectByUUID Failed to get project by uuid err: %+v",
			err)
		return
	}

	return
}

func GetDgraphProjectInfoByUUID(ctx context.Context, projectUUID string, userDgraphUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {

	variables := make(map[string]string)
	variables["$id"] = projectUUID
	variables["$userUid"] = userDgraphUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					uid
					project_uuid
					project_name
					project_status
					project_is_member: count(project_members @filter(uid($userUid)))
					project_is_admin: count(project_admins @filter(uid($userUid)))
					project_team {
						uid
					}
					project_admins {
						user_uuid
					}
					project_tasks (orderasc: task_due_date) {
						task_uuid
						task_name
						task_status
						task_description
						task_assignee {
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_tasks {
							sub_task_uuid
							sub_task_name
							sub_task_assignee {
								user_uuid
								user_name
								user_profile_object_key
							}
							sub_task_due_date
						}
						task_comments {
							comment_uuid
							comment_html_text
							comment_attachments {
								attachment_uuid
								attachment_obj_key
								attachment_file_name
							}
						}
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectInfoByUUID Failed to get project in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateProjectWithAttachmentsInOpensearch(ctx context.Context, openSearchProject *openSearchStruct.OpenSearchProject, dgraphAttachments []*dgraphStruct.DgraphAttachment, userInfo *dgraphStruct.DgraphUser) {

	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachments {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                  attachment.Uuid,
			AttachmentFileName:    attachment.FileName,
			AttachmentByUserUuid:  userInfo.Uuid,
			AttachmentObjKey:      attachment.ObjectKey,
			AttachmentProjectUuid: openSearchProject.Uuid,
			AttachmentCreatedAt:   openSearchProject.ProjectCreatedAt,
		})
	}

	bulkOperationString, err := getProjectWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchProject, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectWithAttachmentsInOpensearch Error getting bulk string for project's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProjectWithAttachmentsInOpensearch Error getting bulk string for project's attachments err: %+v",
			err)
		return
	}

	return
}

// GetDgraphProjectTasksForTemplate returns what a template keeps of a
// project: its live top-level tasks, each with its live subtasks, and how many
// top-level tasks it has. Both lists come in the order the tasks were made
// (uid order), not by task_created_at: a project made from a template has its
// creation times rewritten to read newest first (business/ProjectTemplate
// readingOrder), so sorting by them would turn its plan upside down. first
// caps both lists, so a project too big for a template is found out without
// reading all of it.
func GetDgraphProjectTasksForTemplate(ctx context.Context, projectUUID string, first int) (*dgraphStruct.DgraphProject, error) {
	query := fmt.Sprintf(`query Template($id: string){
			projectInfo(func: eq(project_uuid, $id)) {
				project_uuid
				project_name
				project_task_count: count(project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND NOT has(task_parent_task)))
				project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND NOT has(task_parent_task)) (first: %d) {
					task_uuid
					task_name
					task_description
					task_status
					task_custom_status
					task_custom_status_name
					task_priority
					task_label
					task_start_date
					task_due_date
					task_created_at
					task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) (first: %d) {
						task_name
						task_status
						task_due_date
					}
				}
			}
		}`, first, first)
	return dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, map[string]string{"$id": projectUUID})
}

// GetDgraphProjectTimeline returns what a project's timeline draws: its live
// top-level tasks with their dates, status, priority and assignee, and none of
// the heavier fields (descriptions, comments, GitHub) a list needs. The newest
// first ones when there are more; project_task_count says how many there are.
func GetDgraphProjectTimeline(ctx context.Context, projectUUID, userDgraphUID string, first int) (*dgraphStruct.DgraphProject, error) {
	query := fmt.Sprintf(`query Timeline($id: string, $user: string){
			projectInfo(func: eq(project_uuid, $id)) {
				project_uuid
				project_is_admin: count(project_admins @filter(uid($user)))
				project_task_count: count(project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND NOT has(task_parent_task)))
				project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND NOT has(task_parent_task)) (orderdesc: task_created_at, first: %d) {
					task_uuid
					task_name
					task_status
					task_custom_status
					task_custom_status_name
					task_priority
					task_start_date
					task_due_date
					task_created_at
					task_assignee {
						user_uuid
						user_name
						user_profile_object_key
					}
					task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
					task_blocked_by @facets(kind, lag) @filter(%s) { task_uuid }
					%s
				}
			}
		}`, first, dgraphStruct.TASK_LIVE_FILTER, dgraphStruct.TASK_BLOCKED_OPEN)
	return dgraphModels.GetDgraphProjectInfoByUUID(ctx, query, map[string]string{"$id": projectUUID, "$user": userDgraphUID})
}

// ProjectCounts is one of a person's projects with where its tasks stand, as
// the projects overview shows it and in the same terms as the line under a
// project's name (the app's projectGlance): every live task, subtasks too.
type ProjectCounts struct {
	UUID    string `json:"project_uuid"`
	Name    string `json:"project_name"`
	IsAdmin int    `json:"is_admin"`
	Team    *struct {
		UUID string `json:"team_uuid"`
		Name string `json:"team_name"`
	} `json:"project_team,omitempty"`
	Open    int `json:"open"`
	Done    int `json:"done"`
	Overdue int `json:"overdue"`
	DueSoon int `json:"due_soon"`
	// The days its top-level tasks run across: the earliest start (or due)
	// date to the latest due (or start) date. None without dated tasks.
	FirstDay *time.Time `json:"first_day,omitempty"`
	LastDay  *time.Time `json:"last_day,omitempty"`
}

// projectCountsRow is a ProjectCounts as Dgraph answers it, with the dates
// its tasks start and end on that FirstDay and LastDay are made of.
type projectCountsRow struct {
	ProjectCounts
	MinStart *time.Time `json:"min_start"`
	MaxStart *time.Time `json:"max_start"`
	MinDue   *time.Time `json:"min_due"`
	MaxDue   *time.Time `json:"max_due"`
}

func earliest(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

func latest(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.After(*a)) {
		return b
	}
	return a
}

// GetDgraphProjectCounts returns every live project the person is in, with
// counts of its open, done, overdue (due before today) and due-soon (today to
// the week's end) tasks. today and weekEnd are instants, the start of the
// person's today and seven days on, so days are theirs.
func GetDgraphProjectCounts(ctx context.Context, userDgraphUID string, today, weekEnd time.Time) ([]ProjectCounts, error) {
	const live = dgraphStruct.TASK_LIVE_FILTER
	open := live + ` AND ` + dgraphStruct.TASK_OPEN_FILTER
	top := live + ` AND NOT has(task_parent_task)`
	// The first and last days come from aggregates over the tasks, so the
	// tasks themselves never leave Dgraph.
	query := `query Overview($user: string, $today: string, $weekEnd: string){
			var(func: uid($user)) {
				user_projects {
					project_tasks @filter(` + top + ` AND gt(task_start_date, "1970-01-01T00:00:00Z")) { s as task_start_date }
					min_start as min(val(s))
					max_start as max(val(s))
				}
			}
			var(func: uid($user)) {
				user_projects {
					project_tasks @filter(` + top + ` AND gt(task_due_date, "1970-01-01T00:00:00Z")) { d as task_due_date }
					min_due as min(val(d))
					max_due as max(val(d))
				}
			}
			me(func: uid($user)) {
				user_projects @filter(not gt(project_deleted_at, "1970-01-01T00:00:00Z")) {
					min_start: val(min_start)
					max_start: val(max_start)
					min_due: val(min_due)
					max_due: val(max_due)
					project_uuid
					project_name
					is_admin: count(project_admins @filter(uid($user)))
					project_team { team_uuid team_name }
					open: count(project_tasks @filter(` + open + `))
					done: count(project_tasks @filter(` + live + ` AND eq(task_status, "` + dgraphStruct.TASK_STATUS_DONE + `")))
					overdue: count(project_tasks @filter(` + open + ` AND lt(task_due_date, $today) AND gt(task_due_date, "1970-01-01T00:00:00Z")))
					due_soon: count(project_tasks @filter(` + open + ` AND ge(task_due_date, $today) AND lt(task_due_date, $weekEnd)))
				}
			}
		}`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{
		"$user": userDgraphUID, "$today": today.UTC().Format(time.RFC3339), "$weekEnd": weekEnd.UTC().Format(time.RFC3339),
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphProjectCounts err: %+v", err)
		return nil, err
	}
	var out struct {
		Me []struct {
			Projects []projectCountsRow `json:"user_projects"`
		} `json:"me"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	list := []ProjectCounts{}
	if len(out.Me) == 0 {
		return list, nil
	}
	for _, r := range out.Me[0].Projects {
		c := r.ProjectCounts
		c.FirstDay = earliest(r.MinStart, r.MinDue)
		c.LastDay = latest(r.MaxDue, r.MaxStart)
		list = append(list, c)
	}
	return list, nil
}
