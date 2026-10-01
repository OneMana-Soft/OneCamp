package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Team"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	TeamOSModel "github.com/akashc777/OneCamp/models/openSearch/Team"
	models "github.com/akashc777/OneCamp/models/postgres/Team"
	"github.com/google/uuid"
)

// CREATE TABLE IF NOT EXISTS teams(
// "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
// "team_name" varchar NOT NULL UNIQUE,
// "created_by" uuid REFERENCES users(id),
// "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
// "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
// "deleted_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
// );

func CreateTeam(ctx context.Context, teamUUID uuid.UUID, teamName string, createdByUUID uuid.UUID, createdAt time.Time) (err error) {

	query := `
		INSERT INTO teams (id, team_name, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
	`
	err = models.CreateTeam(query, teamUUID, teamName, createdByUUID, createdAt)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTeam Failed to create new team err: %+v",
			err)
		return
	}
	return
}

func CheckIfTeamExistByTeamName(ctx context.Context, teamName string) (exist bool, err error) {
	query := `
        SELECT EXISTS (
            SELECT 1
            FROM teams
            WHERE team_name = $1
        );
    `

	exist, err = models.CheckIfTeamExistByTeamName(query, teamName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfTeamExistByTeamName Failed to check team by team name err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamNameByTeamUUID(ctx context.Context, newTeamName string, teamUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
        UPDATE teams
        SET team_name = $1, updated_at = $2
		WHERE id = $3`

	err = models.UpdateTeamNameByTeamUUID(query, newTeamName, teamUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamNameByTeamUUID Failed to update team name by team uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamDeletedTimeByUUID(ctx context.Context, teamUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	query := `
        UPDATE teams
        SET deleted_at = $1, updated_at = $2
		WHERE id = $3`
	err = models.UpdateTeamDeletedTimeByUUID(query, teamUUID, deleteTime, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamDeletedTimeByUUID Failed to update team deleted time by team uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamDeletedTimeToNullByUUID(ctx context.Context, teamUUID uuid.UUID, updatedTime time.Time) (err error) {
	query := `
        UPDATE teams
        SET updated_at = $1, deleted_at = null
		WHERE id = $2`
	err = models.UpdateTeamDeletedTimeToNullByUUID(query, teamUUID, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamDeletedTimeToNullByUUID Failed to update delete time to null err: %+v",
			err)
		return
	}

	return
}

func CreateOrUpdateDgraphTeam(ctx context.Context, dgraphTeam *dgraphStruct.DgraphTeam) (teamUid string, err error) {
	dgraphTeam.DType = []string{"Team"}
	query := fmt.Sprintf(`query {
									  team as var(func: eq(team_uuid, "%+v"))
								  }`, dgraphTeam.Uuid)

	teamUid, err = dgraphModels.CreateOrUpdateDgraphTeam(ctx, dgraphTeam, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphTeam Failed to create/update team err: %+v",
			err)
		return
	}
	return
}

func GetBasicDgraphTeamInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUid
	query := `query TeamInfo($id: string, $userUid: string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_created_by {
						uid
						user_uuid
					
					}
					team_deleted_at
					team_is_admin: count(team_admins @filter(uid($userUid)))
					team_is_member: count(team_members @filter(uid($userUid)))
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphTeamInfoByUUID Failed to get team in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx context.Context, teamUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	query := `query TeamInfo($id: string, $userUid:string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_created_by {
						user_uuid
						user_name
						user_email_id
						user_profile_object_key
					}
					team_projects @filter(not gt(project_deleted_at, "1970-01-01T00:00:00Z")){
						project_uuid
						project_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")){
							attachment_uuid
						}
						project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")){
							task_uuid
							task_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")) {
								attachment_uuid
							}
							task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
								comment_uuid

								comment_attachments {
									attachment_uuid
								}

							}
						}
					}
					team_deleted_at
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam Failed to get team in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string, memberUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUid
	variables["$memberUUID"] = memberUUID
	query := `query TeamInfo($id: string, $userUid: string, $memberUUID: string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_created_by {
						uid
						user_uuid
					
					}
					team_admins @filter(eq(user_uuid, $memberUUID)){
						uid
						user_uuid
					}
					team_is_admin: count(team_admins @filter(uid($userUid)))
					team_is_member: count(team_members @filter(uid($userUid)))
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID Failed to get team in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetAllTeamDgraphInfo(ctx context.Context, userDgraphUid string, pageIndex int, pageSize int) (dgraphTeam []*dgraphStruct.DgraphTeam, actualLen int, err error) {
	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$userUid"] = userDgraphUid
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	query := `query TeamInfo($userUid: string, $first: int, $offset: int){
				teamInfo(func: has(team_uuid), first: $first, offset: $offset) {
					team_uuid
					team_name
					team_deleted_at
					team_member_count: count(team_members)
					team_is_member: count(team_members @filter(uid($userUid)))
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAllTeamDgraphInfo Failed to get team list from dgraph err: %+v",
			err,
		)
		return
	}

	actualLen = len(dgraphTeam)
	return
}

func GetBasicDgraphTeamInfoAndMemberInfoByUUID(ctx context.Context, teamUUID string, userDgraphUid string, memberUUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUid
	variables["$memberUUID"] = memberUUID
	query := `query TeamInfo($id: string, $userUid: string, $memberUUID: string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_created_by {
						uid
						user_uuid
					
					}
					team_projects {
						uid
						project_uuid
					}
					team_members @filter(eq(user_uuid, $memberUUID)){
						uid
						user_uuid
					}
					team_is_admin: count(team_admins @filter(uid($userUid)))
					team_is_member: count(team_members @filter(uid($userUid)))
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphTeamInfoByUUID Failed to get team in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphTeamInfoByUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUID
	query := `query TeamInfo($id: string, $userUid:string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_is_admin: count(team_admins @filter(uid($userUid)))
					team_is_member: count(team_members @filter(uid($userUid)))
					team_created_by {
						user_uuid
						user_name
						user_email_id
						user_profile_object_key
					}
					team_created_at
					team_deleted_at
				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChannelInfoByUUID Failed to get team in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userDgraphUID"] = userDgraphUID
	query := `query TeamInfo($id: string, $userDgraphUID: string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					
					team_uuid
					team_name
					team_projects @cascade(project_members) {
						project_uuid
						project_name
						project_status
						project_deleted_at
						project_created_by {
							user_uuid
							user_name
						}
						project_member_count: count(project_members)
						project_members @filter(uid($userDgraphUID)) {
							uid
						}
					}

				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember Failed to get team's project list from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphAllProjectListByTeamUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTeam *dgraphStruct.DgraphTeam, err error) {
	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userDgraphUID"] = userDgraphUID
	query := `query TeamInfo($id: string, $userDgraphUID: string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_projects {
						project_uuid
						project_name
						project_status
						project_deleted_at
						project_created_by {
							user_uuid
							user_name
						}
						project_is_member: count(project_members @filter(uid($userDgraphUID)))
						project_member_count: count(project_members)
					}

				}
			}`

	dgraphTeam, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphAllProjectListByTeamUUID Failed to get team's project list from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphTeamMemberListByTeamUUID(ctx context.Context, teamUUID string, userDgraphUid string) (dgraphTeamInfo *dgraphStruct.DgraphTeam, err error) {
	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUid
	query := `query TeamInfo($id: string, $userUid:string){
				teamInfo(func: eq(team_uuid, $id)) {
					uid
					team_uuid
					team_name
					team_is_admin: count(team_admins @filter(uid($userUid)))
					team_is_member: count(team_members @filter(uid($userUid)))
					team_members @filter(NOT eq(is_external, true)) (orderasc: user_name) {
						user_uuid
						user_name
						user_email
						user_profile_object_key
					}
					team_admins (orderasc: user_name) {
						user_uuid
						
					}
					team_created_by {
						user_uuid
						user_name
						user_email_id
						user_profile_object_key
					}

				}
			}`
	dgraphTeamInfo, err = dgraphModels.GetDgraphTeamInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTeamMemberListByTeamUUID Failed to get team's project list from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphTeamListByUserDgraphUID(ctx context.Context, userDgraphUID string) (dgraphTeamList []*dgraphStruct.DgraphTeam, err error) {
	variables := make(map[string]string)
	variables["$user_id"] = userDgraphUID
	query := `query TeamInfo($user_id: string){
				var(func: uid($user_id)) {
					user_teams {
						t as uid
					}
				}
				teamInfo(func: uid(t)) {
					uid
					team_uuid
					team_name
					team_member_count: count(team_members)
				}
			}`

	dgraphTeamList, err = dgraphModels.GetDgraphTeamList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTeamListByUserDgraphUID Failed to get team list from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphTeamListByAdminDgraphUID(ctx context.Context, userDgraphUID string) (dgraphTeamList []*dgraphStruct.DgraphTeam, err error) {
	variables := make(map[string]string)
	variables["$user_id"] = userDgraphUID
	query := `query TeamInfo($user_id: string){
				var(func: uid($user_id)) {
					~team_admins {
						t as uid
					}
				}
				teamInfo(func: uid(t)) {
					uid
					team_uuid
					team_name

				}
			}`

	dgraphTeamList, err = dgraphModels.GetDgraphTeamList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTeamListByAdminDgraphUID Failed to get team list from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func DeleteTeamAdminMemberEdge(ctx context.Context, teamDgraphUID string, userDgraphUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			{
				"uid": "%s",
				"team_admins": [
					{
						"uid": "%s"
					}
				]
			}
		]
	`, teamDgraphUID, userDgraphUID)

	err = dgraphModels.DeleteTeamEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/ DeleteTeamAdminMemberEdge Failed to remove team admin role from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func DeleteTeamMemberEdge(ctx context.Context, teamDgraph *dgraphStruct.DgraphTeam, userDgraphUID string) (err error) {
	projectList := ""

	if teamDgraph.Projects != nil {
		var userProjects []*dgraphStruct.DgraphProject
		for _, p := range teamDgraph.Projects {
			userProjects = append(userProjects, &dgraphStruct.DgraphProject{Uid: p.Uid})
			projectList += fmt.Sprintf(`
			,{
				"uid": "%s",
				"project_members":[
					{
						"uid": "%s"
					}
				],
				"project_admins":[
					{
						"uid": "%s"
					}
				]
			}
			`, p.Uid, userDgraphUID, userDgraphUID)
		}

		userProjectsJsonData, err := json.Marshal(userProjects)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"domain/DeleteTeamMemberEdge Failed to marshal user projects err: %+v",
				err,
			)
			return err
		}

		projectList += fmt.Sprintf(`
			,{
				"uid": "%s",
				"user_projects":%s
			}
			`, userDgraphUID, string(userProjectsJsonData))
	}

	delStringJSON := fmt.Sprintf(`
		[
			{
				"uid": "%s",
				"team_members": [
					{
						"uid": "%s"
					}
				],
				"team_admins": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"user_teams": [
					{
						"uid": "%s"
					}
				]
			}
			%s
		]
	`, teamDgraph.Uid, userDgraphUID, userDgraphUID, userDgraphUID, teamDgraph.Uid, projectList)

	err = dgraphModels.DeleteTeamEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteTeamMemberEdge Failed to remove team member in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchProject []*openSearchStruct.OpenSearchProject, openSearchAttachments []*openSearchStruct.OpenSearchAttachment, openSearchTasks []*openSearchStruct.OpenSearchTask, openSearchComments []*openSearchStruct.OpenSearchComment) (bulkActionString string, err error) {

	projectUpdate := ""

	for _, project := range openSearchProject {
		tempProjectUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"%+v\", \"_id\": \"%+v\" } }\n", openSearchStruct.PROJECT_INDEX, project.Uuid)

		projectUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: project,
		}
		var projectJsonData []byte
		projectJsonData, err = json.Marshal(projectUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch Error marshiling project struct to json err: %+v",
				err)
			return
		}

		tempProjectUpdate += string(projectJsonData) + "\n"
		projectUpdate += tempProjectUpdate
	}

	bulkActionString += projectUpdate

	attachmentUpdate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentUpdate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.ATTACHMENT_INDEX, attachment.Uuid)
		attachmentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: attachment,
		}

		var attachmentJsonData []byte
		attachmentJsonData, err = json.Marshal(attachmentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch Error marshiling attachment struct to json err: %+v",
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

		var taskJsonData []byte
		taskJsonData, err = json.Marshal(taskUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch Error marshiling task struct to json err: %+v",
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

		var commentJsonData []byte
		commentJsonData, err = json.Marshal(commentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch Error marshiling comment struct to json err: %+v",
				err)
			return
		}

		tempCommentUpdate += string(commentJsonData) + "\n"
		commentUpdate += tempCommentUpdate
	}

	bulkActionString += commentUpdate

	return
}

func UpdateTeamProjectsAndTasksDeleteTimeForOpenSearch(dgraphTeamInfo *dgraphStruct.DgraphTeam, currentTimeUnix int64) {
	ctx := context.Background()
	var openSearchUpdateAttachmentDocs []*openSearchStruct.OpenSearchAttachment
	var openSearchUpdateProjectDocs []*openSearchStruct.OpenSearchProject
	var openSearchUpdateTaskDocs []*openSearchStruct.OpenSearchTask
	var openSearchUpdateCommentDocs []*openSearchStruct.OpenSearchComment

	for _, project := range dgraphTeamInfo.Projects {

		openSearchUpdateProjectDocs = append(openSearchUpdateProjectDocs, &openSearchStruct.OpenSearchProject{
			Uuid:             project.Uuid,
			ProjectDeletedAt: helpers.Int64Pointer(currentTimeUnix),
		})

		for _, attachment := range project.Attachments {

			openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
				Uuid:                attachment.Uuid,
				AttachmentDeletedAt: helpers.Int64Pointer(currentTimeUnix),
			})

		}

		for _, task := range project.Tasks {
			openSearchUpdateTaskDocs = append(openSearchUpdateTaskDocs, &openSearchStruct.OpenSearchTask{
				Uuid:          task.Uuid,
				TaskDeletedAt: helpers.Int64Pointer(currentTimeUnix),
			})

			for _, attachment := range task.Attachments {
				openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
					Uuid:                attachment.Uuid,
					AttachmentDeletedAt: helpers.Int64Pointer(currentTimeUnix),
				})
			}

			for _, comment := range task.Comments {
				openSearchUpdateCommentDocs = append(openSearchUpdateCommentDocs, &openSearchStruct.OpenSearchComment{
					Uuid:             comment.Uuid,
					CommentDeletedAt: helpers.Int64Pointer(currentTimeUnix),
				})
				for _, attachment := range comment.Attachments {
					openSearchUpdateAttachmentDocs = append(openSearchUpdateAttachmentDocs, &openSearchStruct.OpenSearchAttachment{
						Uuid:                attachment.Uuid,
						AttachmentDeletedAt: helpers.Int64Pointer(currentTimeUnix),
					})
				}

			}
		}

	}

	bulkOperationString, err := getUpdateTeamProjecsTasksCommentsAndAttachmentBulkOperationStringForOpenSearch(ctx, openSearchUpdateProjectDocs, openSearchUpdateAttachmentDocs, openSearchUpdateTaskDocs, openSearchUpdateCommentDocs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamProjectsAndTasksDeleteTimeForOpenSearch Error getting bulk update string err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTeamProjectsAndTasksDeleteTimeForOpenSearch Error in bulk err: %+v",
			err)
		return
	}

}

func CreateTeamInOpenSearch(openSearchTeam *openSearchStruct.OpenSearchTeam) (err error) {
	ctx := context.Background()
	return TeamOSModel.CreateTeamInOpenSearch(ctx, openSearchTeam)
}

func UpdateTeamInOpenSearch(openSearchTeam *openSearchStruct.OpenSearchTeam) (err error) {
	ctx := context.Background()

	return TeamOSModel.UpdateTeamInOpenSearch(ctx, openSearchTeam)
}

func DeleteTeamInOpenSearch(teamUUID string, deletedAt int64) (err error) {
	ctx := context.Background()

	return TeamOSModel.DeleteTeamInOpenSearch(ctx, teamUUID, deletedAt)
}

// HardDeleteTeam removes a team row outright.
//
// Compensation only: see models.HardDeleteTeam for why a soft delete cannot serve here
// (team_name is UNIQUE and a soft-deleted row keeps the name).
func HardDeleteTeam(ctx context.Context, teamUUID uuid.UUID) (err error) {
	query := `DELETE FROM teams WHERE id = $1`
	err = models.HardDeleteTeam(query, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteTeam Failed to delete team row err: %+v", err)
		return
	}
	return
}
