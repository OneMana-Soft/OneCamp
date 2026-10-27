package Domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Task"
	models "github.com/akashc777/OneCamp/models/postgres/Task"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

//CREATE TABLE IF NOT EXISTS tasks(
//"id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
//"task_name" varchar NOT NULL,
//"project_id" uuid REFERENCES teams(id),
//"created_by" uuid REFERENCES users(id),
//"task_assignee" uuid REFERENCES users(id),
//"created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"deleted_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
//);

const TASK_COUNT = 10
const TASK_STATUS_COMPLETED = "completed"
const TASK_STATUS_UNCOMPLETED = "uncompleted"

// CreateTask makes a task's row in its project and gives it the project's
// next number (business/TaskKeys): answers the number, or 0 when the project
// has no key yet, which the caller settles (business/TaskKeys Settle).
func CreateTask(ctx context.Context, taskUUID uuid.UUID, projectUUID uuid.UUID, createdByUUID uuid.UUID, createdAt time.Time) (number int, err error) {
	number, err = taskKeyModel.InsertTask(ctx, taskUUID, projectUUID, createdByUUID, createdAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTask Failed to create new task err: %+v",
			err)
		return 0, err
	}
	return number, nil
}

// CreateTaskNumbered is CreateTask keeping a number the task had in another
// tool (an import's ENG-123) when the project hasn't used it.
func CreateTaskNumbered(ctx context.Context, taskUUID, projectUUID, createdByUUID uuid.UUID, createdAt time.Time, number int) (int, error) {
	n, err := taskKeyModel.InsertTaskNumbered(ctx, taskUUID, projectUUID, createdByUUID, createdAt, number)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateTaskNumbered Failed to create new task err: %+v", err)
	}
	return n, err
}

// func CreateTaskWithAssignee(taskUUID uuid.UUID, taskAssigneeUUID uuid.UUID, projectUUID uuid.UUID, createdByUUID uuid.UUID, createdAt time.Time) (err error) {

// 	query := `
// 		INSERT INTO tasks (id, project_id, task_assignee, created_by, created_at, updated_at)
// 		VALUES ($1, $2, $3, $4)
// 	`
// 	err = models.CreateTaskWithAssignee(query, taskUUID, projectUUID, taskAssigneeUUID, createdByUUID, createdAt)

// 	if err != nil {
// 		helpers.LogErrorWithContext(ctx,
// 			"domain/CreateTaskWithAssignee Failed to create new task err: %+v",
// 			err)
// 		return
// 	}
// 	return
// }

//func CheckIfTaskExistByTaskNameAndTeamID(taskName string, projectUUID uuid.UUID) (exist bool, err error) {
//	query := `
//        SELECT EXISTS (
//            SELECT 1
//            FROM projects
//            WHERE project_name = $1
//        );
//    `
//
//	exist, err = models.CheckIfProjectExistByProjectNameAndTeamUUID(query, projectName, teamUUID)
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"domain/CheckIfProjectExistByProjectNameAndTeamID Failed to check project by project name and teamID err: %+v",
//			err)
//		return
//	}
//
//	return
//}

func GetTaskByUUID(ctx context.Context, taskUUID uuid.UUID) (taskInfo *models.Task, err error) {

	query := `
        SELECT id, created_by, project_id, task_assignee, created_at, updated_at, deleted_at, task_google_calendar_id
        FROM tasks
        WHERE id = $1
    `

	taskInfo, err = models.GetTaskByUUID(ctx, query, taskUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetTaskByUUID Failed to get task by uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateTaskByTaskUUID(ctx context.Context, taskUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
        UPDATE tasks
        SET updated_at = $1
		WHERE id = $2`

	err = models.UpdateTaskNameByTaskUUID(ctx, query, currentTime, taskUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskNameByTaskUUID Failed to update task name by task uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateTaskDeletedTimeByUUID(ctx context.Context, taskUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	query := `
        UPDATE tasks
        SET deleted_at = $1, updated_at = $2
		WHERE id = $3`
	err = models.UpdateTaskDeletedTimeByUUID(ctx, query, taskUUID, deleteTime, updateTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskDeletedTimeByUUID Failed to update task delete time by task uuid err: %+v",
			err)
		return
	}

	return
}
func UpdateTaskGoogleCalendarId(ctx context.Context, taskUUID uuid.UUID, googleEventId *string) error {
	currentTime := time.Now()
	query := `
		UPDATE tasks
		SET task_google_calendar_id = $1, updated_at = $2
		WHERE id = $3
	`
	// I'll reuse UpdateTaskNameByTaskUUID model or add a new one.
	// Since UpdateTaskNameByTaskUUID takes projectUUID but queries by id, it's actually a generic update.
	// No, wait, let's look at taskModel.go:138. It takes projectUUID but uses it as $2.
	return models.UpdateTaskGoogleCalendarId(ctx, query, googleEventId, currentTime, taskUUID)
}

func GetSyncedTaskGCalIds(ctx context.Context, userUUID uuid.UUID) ([]string, error) {
	return models.GetSyncedTaskGCalIds(ctx, userUUID)
}

func UpdateTaskDeletedTimeToNullByUUID(ctx context.Context, projectUUID uuid.UUID, updatedTime *time.Time) (err error) {
	query := `
        UPDATE tasks
        SET updated_at = $1, deleted_at = null
		WHERE id = $2`
	err = models.UpdateTaskDeletedTimeToNullByUUID(ctx, query, updatedTime, projectUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskDeletedTimeToNullByUUID Failed to update delete time to null err: %+v",
			err)
		return
	}
	return
}

// func UpdateTaskAssigneeByTaskUUID(taskUUID uuid.UUID, taskAssignee uuid.UUID, updatedTime *time.Time) (err error) {
// 	query := `
//         UPDATE tasks
//         SET task_assignee = $1, updated_at = $2
// 		WHERE id = $3`
// 	err = models.UpdateTaskAssigneeByTaskUUID(query, updatedTime, taskAssignee, taskUUID)
// 	if err != nil {
// 		helpers.LogErrorWithContext(ctx,
// 			"domain/UpdateTaskAssigneeByTaskUUID Failed to update task assignee err: %+v",
// 			err)
// 		return
// 	}
// 	return
// }

func UpdateDgraphTaskAssignee(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask, dgraphUserUID string, dgraphTaskUID string) (taskUid string, err error) {

	delStringJSON := ""

	if len(dgraphUserUID) > 0 {
		delStringJSON = fmt.Sprintf(`
		[
			{
				"uid": "%s",
				"user_tasks": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "uid(task)",
				"task_assignee": null
			}
	]
	`, dgraphUserUID, dgraphTaskUID)
	}

	dgraphTask.DType = []string{"task"}
	query := fmt.Sprintf(`query {
									  task as var(func: eq(task_uuid, "%+v"))
								  }`, dgraphTask.Uuid)

	taskUid, err = dgraphModels.CreateOrUpdateDgraphTask(ctx, dgraphTask, query, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDgraphTaskAssignee Failed to create/update task err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphTaskLabel(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask) (taskUid string, err error) {

	dgraphTask.DType = []string{"Task"}
	query := fmt.Sprintf(`query {
									  task as var(func: eq(task_uuid, "%+v"))
								  }`, dgraphTask.Uuid)

	taskUid, err = dgraphModels.CreateOrUpdateDgraphTask(ctx, dgraphTask, query, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphTaskLabel Failed to create/update task err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphTaskDescWithMentions(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask) (taskUid string, err error) {

	dgraphTask.DType = []string{"Task"}
	query := fmt.Sprintf(`query {
									  task as var(func: eq(task_uuid, "%+v"))
									  me as var(func: eq(mention_task_uuid, %+v))
								  }`, dgraphTask.Uuid, dgraphTask.Uuid)

	delStringJSON := `{
		"uid": "uid(me)",
		"mention_users": null
	}`

	taskUid, err = dgraphModels.CreateOrUpdateDgraphTask(ctx, dgraphTask, query, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphTaskDesc Failed to create/update task err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphTask(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask) (taskUid string, err error) {

	dgraphTask.DType = []string{"Task"}
	query := fmt.Sprintf(`query {
									  task as var(func: eq(task_uuid, "%+v"))
								  }`, dgraphTask.Uuid)

	taskUid, err = dgraphModels.CreateOrUpdateDgraphTask(ctx, dgraphTask, query, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphTask Failed to create/update task err: %+v",
			err)
		return
	}
	return
}

func UpdateTaskInOpenSearch(openSearchTask *openSearchStruct.OpenSearchTask) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateTaskInOpenSearch(ctx, openSearchTask)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskInOpenSearch Failed to update task in opensearch err: %+v",
			err)
		return
	}
	return
}

func getUpdateTaskWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchTask *openSearchStruct.OpenSearchTask, openSearchAttachment []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	taskUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"%+v\", \"_id\": \"%+v\" } }\n", openSearchStruct.TASK_INDEX, openSearchTask.Uuid)
	taskUpdateDoc := &openSearchStruct.BulkUpdate{
		Doc: openSearchTask,
	}
	taskJsonData, err := json.Marshal(taskUpdateDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getUpdateTaskWithAttachmentBulkOperationStringForOpenSearch Error mashiling task struct to json err: %+v",
			err)
		return
	}

	taskUpdate += string(taskJsonData) + "\n"

	bulkActionString += taskUpdate

	attachmentCreate := ""

	for _, attachment := range openSearchAttachment {
		tempAttachmentCreate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: attachment,
		}
		attachmentJsonData, _ := json.Marshal(attachmentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getUpdateTaskWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func UpdateTaskWithAttachmentsInOpenSearch(openSearchTask *openSearchStruct.OpenSearchTask, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchUpdateDocs []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchUpdateDocs = append(openSearchUpdateDocs, &openSearchStruct.OpenSearchAttachment{
			Uuid: attachment.Uuid,
		})
	}

	bulkOperationString, err := getUpdateTaskWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchTask, openSearchUpdateDocs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskWithAttachmentsInOpenSearch Error getting bulk string for task's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateTaskWithAttachmentsInOpenSearch Error getting bulk string for task with attachments err: %+v",
			err)
		return
	}

}

func CreateTaskAttachmentsInOpensearch(currentTime *time.Time, dgraphTaskInfo *dgraphStruct.DgraphTask, dgraphAttachments []*dgraphStruct.DgraphAttachment, userInfo *dgraphStruct.DgraphUser) {

	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachments {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                  attachment.Uuid,
			AttachmentFileName:    attachment.FileName,
			AttachmentByUserUuid:  userInfo.Uuid,
			AttachmentObjKey:      attachment.ObjectKey,
			AttachmentTaskUuid:    dgraphTaskInfo.Uuid,
			AttachmentProjectUuid: dgraphTaskInfo.Project.Uuid,
			AttachmentCreatedAt:   currentTime.Unix(),
		})
	}

	bulkOperationString, err := getTaskWithAttachmentBulkOperationStringForOpenSearch(ctx, nil, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateAttachmentsInOpensearch Error getting bulk string for tasks's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateAttachmentsInOpensearch Error getting bulk string for task with attachments err: %+v",
			err)
		return
	}

	return

}

func CreateTaskWithAttachmentsInOpensearch(openSearchTask *openSearchStruct.OpenSearchTask, dgraphAttachments []*dgraphStruct.DgraphAttachment, userInfo *dgraphStruct.DgraphUser) {

	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachments {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                  attachment.Uuid,
			AttachmentFileName:    attachment.FileName,
			AttachmentByUserUuid:  userInfo.Uuid,
			AttachmentObjKey:      attachment.ObjectKey,
			AttachmentTaskUuid:    openSearchTask.Uuid,
			AttachmentProjectUuid: openSearchTask.TaskProjectUuid,
			AttachmentCreatedAt:   openSearchTask.TaskCreatedAt,
		})
	}

	bulkOperationString, err := getTaskWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchTask, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTaskWithAttachmentsInOpensearch Error getting bulk string for tasks's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTaskWithAttachmentsInOpensearch Error getting bulk string for task's' attachments err: %+v",
			err)
		return
	}

	return

}

func getTaskWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchTask *openSearchStruct.OpenSearchTask, openSearchAttachments []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	if openSearchTask != nil {
		postCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"%+v\", \"_id\" : \"%+v\" } }\n", openSearchStruct.TASK_INDEX, openSearchTask.Uuid)
		var postJsonData []byte
		postJsonData, err = json.Marshal(openSearchTask)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"domain/getTaskWithAttachmentBulkOperationStringForOpenSearch Error mashiling task struct to json err: %+v",
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
				"models/getTaskWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func GetDgraphBasicTaskInfoWithAttachmentsByUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	// A background read has no reader and passes "". Dgraph refuses an empty
	// uid() and fails the whole query ("ID can't be empty"), which is how every
	// agent work event silently lost its project. The membership counts it feeds
	// are not consulted for a system read, so any uid that is not a person works.
	if strings.TrimSpace(userDgraphUID) == "" {
		userDgraphUID = "0x1"
	}
	variables["$userUid"] = userDgraphUID
	query := `query TaskInfo($id: string, $userUid: string){
				taskInfo(func: eq(task_uuid, $id)) {
				
					task_uuid
					task_name
					task_status
					task_custom_status
					task_custom_status_name
					task_priority
					task_label
					task_start_date
					task_due_date
					task_description
					task_team {
						team_name
						team_uuid
					}
					task_project {
						uid
						project_uuid
						project_name
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid))) 

					}
					task_attachments {
						attachment_uuid
						attachment_obj_key
						attachment_file_name
					}
					task_assignee {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					task_parent_task {
						uid
						task_uuid
						task_name
					}
					task_created_by {
						uid
						user_uuid
					}
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphBasicTaskInfoWithAttachmentsByUUID Failed to get task in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphBasicTaskInfoByUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUID
	query := `query TaskInfo($id: string, $userUid: string){
				taskInfo(func: eq(task_uuid, $id)) {
					uid
					task_uuid
					task_name
					task_status
					task_custom_status
					task_custom_status_name
					task_priority
					task_label
					task_start_date
					task_due_date
					task_deleted_at
					task_estimate_minutes
					task_description
					task_team {
						team_name
						team_uuid
					}
					task_attachments {
						uid
						attachment_uuid
						attachment_file_name
					}
					task_project {
						uid
						project_uuid
						project_name
						project_deleted_at
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid))) 

					}
					task_assignee {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					task_parent_task {
						task_uuid
						task_name
					}
					task_created_by {
						uid
						user_uuid
					}
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphBasicTaskInfoByUUID Failed to get task in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// TaskHistoryMax is the most history lines one read of a task gives: a
// task's panel carries its newest TaskHistoryMax, and GetDgraphTaskActivityList
// pages back from there.
const TaskHistoryMax = 1000

// ErrUnknownHistoryLine is a page of history asked for before a line the task
// doesn't have.
var ErrUnknownHistoryLine = errors.New("that history line isn't this task's")

const historyLineFields = `
						activity_uuid
						activity_by {
							user_uuid
							user_name
							user_profile_object_key
						}
						activity_time
						activity_type
						activity_next_state`

// taskHistoryBlock is the DQL, inside a task's block, for a page of its
// history: newest first, by time then uuid so lines from one save keep their
// order across pages, first+1 lines so settleHistory can tell whether older
// ones are left, only those older than the line ($beforeTime, $beforeUUID)
// when before is set; and how many lines the task has. It was sorted oldest
// first with no first, which Dgraph stops at 1,000, so a long history lost
// its newest lines.
func taskHistoryBlock(first int, before bool) string {
	filter := ""
	if before {
		filter = ` @filter(lt(activity_time, $beforeTime) OR (eq(activity_time, $beforeTime) AND lt(activity_uuid, $beforeUUID)))`
	}
	return fmt.Sprintf(`
					task_activity_count: count(task_activities)
					task_activities%s (orderdesc: activity_time, orderdesc: activity_uuid, first: %d) {%s
					}`, filter, first+1, historyLineFields)
}

// settleHistory makes a taskHistoryBlock answer the page it asked for: at
// most first lines, oldest first as a panel shows them, and whether older
// lines are left.
func settleHistory(t *dgraphStruct.DgraphTask, first int) {
	if t == nil {
		return
	}
	if len(t.Activity) > first {
		t.Activity = t.Activity[:first]
		t.ActivityHasMore = true
	}
	slices.Reverse(t.Activity)
}

// GetDgraphTaskActivityList is a page of a task's history: its newest first
// lines (1..TaskHistoryMax), or those just older than the line
// beforeActivityUUID when it is set, oldest first; with the line count,
// whether older lines are left, and the viewer's membership of the project.
func GetDgraphTaskActivityList(ctx context.Context, taskUUID string, userDgraphUID string, beforeActivityUUID string, first int) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	first = min(max(first, 1), TaskHistoryMax)
	variables := map[string]string{"$id": taskUUID, "$userUid": userDgraphUID}
	params := "$id: string, $userUid: string"
	if beforeActivityUUID != "" {
		at, member, err := taskHistoryLineTime(ctx, taskUUID, beforeActivityUUID, userDgraphUID)
		if err != nil {
			return nil, err
		}
		if !member {
			// Nothing is said about the line to someone outside the project:
			// the caller refuses them, as it does any read of the history.
			return &dgraphStruct.DgraphTask{Uuid: taskUUID, Project: &dgraphStruct.DgraphProject{}}, nil
		}
		if at == "" {
			return nil, ErrUnknownHistoryLine
		}
		variables["$beforeTime"], variables["$beforeUUID"] = at, beforeActivityUUID
		params += ", $beforeTime: string, $beforeUUID: string"
	}
	query := `query TaskInfo(` + params + `){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid` + taskHistoryBlock(first, beforeActivityUUID != "") + `
					task_project {
						uid
						project_uuid
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid)))
					}
					task_assignee { uid }
					task_created_by { uid }
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTaskActivityList Failed to get task in dgraph err: %+v",
			err,
		)
		return nil, err
	}
	settleHistory(dgraphTask, first)
	return dgraphTask, nil
}

// taskHistoryLineTime is when the task's history line activityUUID was
// written, as Dgraph gives it ("" when the task has no such line), and
// whether userDgraphUID is a member of the task's project.
func taskHistoryLineTime(ctx context.Context, taskUUID, activityUUID, userDgraphUID string) (at string, member bool, err error) {
	// Who reads the history is who reads the task (TaskReader): the people in
	// its project (a member or an admin) and its assignee.
	query := `query Line($id: string, $line: string, $userUid: string){
				t(func: eq(task_uuid, $id)) {
					task_project {
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid)))
					}
					task_is_assignee: count(task_assignee @filter(uid($userUid)))
					task_activities @filter(eq(activity_uuid, $line)) {
						activity_time
					}
				}
			}`
	var out struct {
		T []struct {
			Project struct {
				Member int `json:"project_is_member"`
				Admin  int `json:"project_is_admin"`
			} `json:"task_project"`
			Assignee int `json:"task_is_assignee"`
			Lines    []struct {
				At string `json:"activity_time"`
			} `json:"task_activities"`
		} `json:"t"`
	}
	if err := dgraphInit.ReadInto(ctx, query, map[string]string{"$id": taskUUID, "$line": activityUUID, "$userUid": userDgraphUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/taskHistoryLineTime err: %+v", err)
		return "", false, err
	}
	if len(out.T) == 0 {
		return "", false, nil
	}
	if len(out.T[0].Lines) > 0 {
		at = out.T[0].Lines[0].At
	}
	t := out.T[0]
	return at, t.Project.Member > 0 || t.Project.Admin > 0 || t.Assignee > 0, nil
}

func GetDgraphTaskActivityInfo(ctx context.Context, teamUUID string, activityUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	variables["$userUid"] = userDgraphUID
	variables["$activityUUID"] = activityUUID
	query := `query TaskInfo($id: string, $userUid: string, $activityUUID: string){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid
					task_activities @filter(eq(activity_uuid, $activityUUID)) {
						activity_uuid
						activity_by {
							user_uuid
							user_name
							user_profile_object_key
						}
						activity_time
						activity_type
						activity_next_state
					}
					task_project {
						uid
						project_uuid
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid)))
					}
					task_assignee { uid }
					task_created_by { uid }
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTaskActivityInfo Failed to get task in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphTaskInfoByUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	variables := make(map[string]string)
	variables["$id"] = teamUUID
	// The asker's standing in the project, which CanViewTask reads. A system
	// read has nobody asking (userDgraphUID empty) and counts none: Dgraph
	// refuses uid(""), which failed the read for every such caller.
	params, standing := "$id: string", ""
	if userDgraphUID != "" {
		variables["$userUid"] = userDgraphUID
		params += ", $userUid: string"
		standing = `project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid)))`
	}
	query := `query TaskInfo(` + params + `){
				taskInfo(func: eq(task_uuid, $id)) {
					uid
					task_uuid
					task_name
					task_status
					task_custom_status
					task_custom_status_name
					task_priority
					task_label
					task_start_date
					task_due_date
					task_description
					task_deleted_at
					task_team {
						team_name
						team_uuid
					}
					task_attachments @filter(not gt(attachment_deleted_at, "1970-01-01T00:00:00Z")){
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
					task_project {
						uid
						project_uuid
						project_name
						project_deleted_at
						` + standing + `
						project_members {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}

					}
					task_assignee {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				task_github_issue_number
				task_github_issue_url
				task_github_pr_number
				task_github_pr_url
				task_github_branch
				task_estimate_minutes
				task_parent_task {
					task_uuid
					task_name
				}
				task_blocked_by @facets(kind, lag) @filter(` + dgraphStruct.TASK_LIVE_FILTER + `) {
					` + dependencyFields + `
				}
				task_blocks: ~task_blocked_by @facets(kind, lag) @filter(` + dgraphStruct.TASK_LIVE_FILTER + `) {
					` + dependencyFields + `
				}
				task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")){
						task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_priority
						task_label
						task_start_date
						task_due_date
						task_description
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
					}
					task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
						comment_text
						comment_attachments {
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
						comment_reactions {
							uid
							reaction_emoji_id
							reaction_added_by {
								user_uuid
								user_name
							}
						}
						comment_by {
							user_uuid
							user_profile_object_key
							user_name
						}
						comment_created_at
					}
						
					` + taskHistoryBlock(TaskHistoryMax, false) + `
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, variables)
	settleHistory(dgraphTask, TaskHistoryMax)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTaskInfoByUUID Failed to get task in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// SetGitHubPRFieldsOnTask sets PR metadata on a specific task.
func SetGitHubPRFieldsOnTask(ctx context.Context, taskUUID uuid.UUID, prNumber int, prURL string, branch string) error {
	query := `UPDATE tasks SET github_pr_number = $1, github_pr_url = $2, github_branch = $3, github_last_synced_at = NOW(), updated_at = NOW() WHERE id = $4`
	err := models.SetGitHubPRFieldsOnTask(ctx, query, taskUUID, prNumber, prURL, branch)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubPRFieldsOnTask Failed err: %+v", err)
		return err
	}
	return nil
}

// UpdateTaskPRInfo updates only PR number and URL for a task.
func UpdateTaskPRInfo(ctx context.Context, taskUUID uuid.UUID, prNumber int, prURL string) error {
	query := `UPDATE tasks SET github_pr_number = $1, github_pr_url = $2, github_last_synced_at = NOW(), updated_at = NOW() WHERE id = $3`
	err := models.UpdateTaskPRInfo(ctx, query, taskUUID, prNumber, prURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateTaskPRInfo Failed err: %+v", err)
		return err
	}
	return nil
}

// FindTaskUUIDByPRURL finds a task by its GitHub PR URL.
func FindTaskUUIDByPRURL(ctx context.Context, prURL string) (taskUUID string, err error) {
	query := `SELECT id::text FROM tasks WHERE github_pr_url = $1 AND deleted_at IS NULL LIMIT 1`
	taskUUID, err = models.FindTaskUUIDByPRURL(ctx, query, prURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FindTaskUUIDByPRURL Failed err: %+v", err)
		return "", err
	}
	return taskUUID, nil
}

// SetGitHubIssueFieldsOnTask sets GitHub issue metadata on a specific task.
func SetGitHubIssueFieldsOnTask(ctx context.Context, taskUUID uuid.UUID, issueNumber int, issueURL string) error {
	query := `UPDATE tasks SET github_issue_number = $1, github_issue_url = $2, github_last_synced_at = NOW() WHERE id = $3`
	err := models.SetGitHubIssueFieldsOnTask(ctx, query, taskUUID, issueNumber, issueURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubIssueFieldsOnTask Failed err: %+v", err)
		return err
	}
	return nil
}

// FindTaskUUIDByGitHubIssueURL finds a task by its GitHub issue URL.
func FindTaskUUIDByGitHubIssueURL(ctx context.Context, issueURL string) (string, error) {
	query := `SELECT id::text FROM tasks WHERE github_issue_url = $1 AND deleted_at IS NULL LIMIT 1`
	taskUUID, err := models.FindTaskUUIDByGitHubIssueURL(ctx, query, issueURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FindTaskUUIDByGitHubIssueURL Failed err: %+v", err)
		return "", err
	}
	return taskUUID, nil
}

// FindTasksByGitHubIssueURLs batches the per-URL existence check used
// by the import path. Returns a map of issue_url -> task_uuid for
// every URL already linked to a non-deleted task. Missing keys mean
// the issue has not yet been imported.
//
// One DB roundtrip regardless of input size (Postgres ANY-array
// expansion). The caller should still cap the slice length to a
// reasonable number (we recommend a few thousand) to avoid driver
// param limits, but for the typical import batch of a single
// per_page=100 page this is one query per page instead of N.
func FindTasksByGitHubIssueURLs(ctx context.Context, urls []string) (map[string]string, error) {
	if len(urls) == 0 {
		return map[string]string{}, nil
	}
	query := `SELECT github_issue_url, id::text FROM tasks WHERE github_issue_url = ANY($1) AND deleted_at IS NULL`
	res, err := models.FindTasksUUIDByGitHubURLs(ctx, query, urls)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FindTasksByGitHubIssueURLs Failed err: %+v", err)
		return nil, err
	}
	return res, nil
}

// FindTasksByGitHubPRURLs is the PR-URL twin of FindTasksByGitHubIssueURLs.
func FindTasksByGitHubPRURLs(ctx context.Context, urls []string) (map[string]string, error) {
	if len(urls) == 0 {
		return map[string]string{}, nil
	}
	query := `SELECT github_pr_url, id::text FROM tasks WHERE github_pr_url = ANY($1) AND deleted_at IS NULL`
	res, err := models.FindTasksUUIDByGitHubURLs(ctx, query, urls)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FindTasksByGitHubPRURLs Failed err: %+v", err)
		return nil, err
	}
	return res, nil
}

// GetTaskLastSyncedAtByIssueURL returns the last sync time for a task by issue URL.
func GetTaskLastSyncedAtByIssueURL(ctx context.Context, issueURL string) (*time.Time, error) {
	query := `SELECT github_last_synced_at FROM tasks WHERE github_issue_url = $1 LIMIT 1`
	lastSyncedAt, err := models.GetTaskLastSyncedAtByIssueURL(ctx, query, issueURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTaskLastSyncedAtByIssueURL Failed err: %+v", err)
		return nil, err
	}
	return lastSyncedAt, nil
}

// GetTaskLastSyncedAtByPRURL returns the last sync time for a task by PR URL.
func GetTaskLastSyncedAtByPRURL(ctx context.Context, prURL string) (*time.Time, error) {
	query := `SELECT github_last_synced_at FROM tasks WHERE github_pr_url = $1 LIMIT 1`
	lastSyncedAt, err := models.GetTaskLastSyncedAtByPRURL(ctx, query, prURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTaskLastSyncedAtByPRURL Failed err: %+v", err)
		return nil, err
	}
	return lastSyncedAt, nil
}

// ClearGitHubIssueFieldsFromTask clears GitHub issue fields from a task.
func ClearGitHubIssueFieldsFromTask(ctx context.Context, taskUUID uuid.UUID) error {
	query := `UPDATE tasks SET github_issue_number = NULL, github_issue_url = NULL, github_last_synced_at = NULL, updated_at = NOW() WHERE id = $1`
	err := models.ClearGitHubIssueFieldsFromTask(ctx, query, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ClearGitHubIssueFieldsFromTask Failed err: %+v", err)
		return err
	}
	return nil
}

// ClearGitHubLinkFromTask unlinks a task from GitHub: its issue, and its pull
// request with that pull request's branch and state. Clearing only the issue
// left a pull request linked, which the sync went on posting the task to.
func ClearGitHubLinkFromTask(ctx context.Context, taskUUID uuid.UUID) error {
	query := `UPDATE tasks SET github_issue_number = NULL, github_issue_url = NULL,
		github_pr_number = NULL, github_pr_url = NULL, github_branch = NULL, github_pr_state = NULL,
		github_pr_check_status = NULL, github_pr_review_state = NULL, github_pr_is_draft = false,
		github_last_synced_at = NULL, updated_at = NOW() WHERE id = $1`
	err := models.ClearGitHubIssueFieldsFromTask(ctx, query, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ClearGitHubLinkFromTask Failed err: %+v", err)
		return err
	}
	return nil
}

// GetTaskGitHubURLs returns the GitHub issue and PR URLs for a task.
func GetTaskGitHubURLs(ctx context.Context, taskUUID uuid.UUID) (issueURL, prURL *string, err error) {
	query := `SELECT github_issue_url, github_pr_url FROM tasks WHERE id = $1`
	issueURL, prURL, err = models.GetTaskGitHubURLs(ctx, query, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTaskGitHubURLs Failed err: %+v", err)
		return nil, nil, err
	}
	return issueURL, prURL, nil
}

// FindTaskUUIDByGitHubBranch finds the task on branch of a project that links
// the repository owner/name, matched in any case as GitHub matches names. A
// branch's name is whoever pushes it's to choose ("main", "patch-1"), so a
// task of a project that doesn't link the repository is never the one a
// branch there means.
func FindTaskUUIDByGitHubBranch(ctx context.Context, owner, name, branch string) (string, error) {
	query := `SELECT t.id FROM tasks t
		WHERE t.github_branch = $3 AND t.deleted_at IS NULL AND t.project_id IS NOT NULL
		AND t.project_id IN (SELECT l.project_id FROM github_links l
			WHERE lower(l.repo_owner) = lower($1) AND lower(l.repo_name) = lower($2) AND l.deleted_at IS NULL)
		LIMIT 1`
	taskUUID, err := models.FindTaskUUIDByGitHubBranch(ctx, query, owner, name, branch)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FindTaskUUIDByGitHubBranch Failed err: %+v", err)
		return "", err
	}
	return taskUUID, nil
}

// SetGitHubLastSyncedAt updates the last sync timestamp for a task.
func SetGitHubLastSyncedAt(ctx context.Context, taskUUID uuid.UUID, t time.Time) error {
	query := `UPDATE tasks SET github_last_synced_at = $1 WHERE id = $2`
	err := models.SetGitHubLastSyncedAt(ctx, query, taskUUID, t)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubLastSyncedAt Failed err: %+v", err)
		return err
	}
	return nil
}

// SetGitHubBranchOnTaskByTaskID sets the github_branch on a specific task by UUID.
func SetGitHubBranchOnTaskByTaskID(ctx context.Context, branchName string, taskUUID uuid.UUID) error {
	query := `UPDATE tasks SET github_branch = $1, updated_at = NOW() WHERE id = $2`
	err := models.SetGitHubBranchOnTaskByTaskID(ctx, query, branchName, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubBranchOnTaskByTaskID Failed err: %+v", err)
		return err
	}
	return nil
}

// UpdateTaskPRState updates the GitHub PR state fields on a task.
// Uses CASE to preserve existing values when an empty string or nil is passed for a field.
func UpdateTaskPRState(ctx context.Context, taskUUID uuid.UUID, prState string, checkStatus string, reviewState string, isDraft *bool) error {
	query := `
		UPDATE tasks SET
			github_pr_state = CASE WHEN $1 = '' THEN github_pr_state ELSE $1 END,
			github_pr_check_status = CASE WHEN $2 = '' THEN github_pr_check_status ELSE $2 END,
			github_pr_review_state = CASE WHEN $3 = '' THEN github_pr_review_state ELSE $3 END,
			github_pr_is_draft = CASE WHEN $4 IS NULL THEN github_pr_is_draft ELSE $4 END,
			updated_at = NOW()
		WHERE id = $5
	`
	err := models.UpdateTaskPRState(ctx, query, taskUUID, prState, checkStatus, reviewState, isDraft)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateTaskPRState Failed err: %+v", err)
		return err
	}
	return nil
}

// GetTaskProjectID returns the project UUID for a task.
func GetTaskProjectID(ctx context.Context, taskUUID uuid.UUID) (uuid.UUID, error) {
	projectID, err := models.GetTaskProjectID(ctx, taskUUID, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTaskProjectID Failed err: %+v", err)
		return uuid.Nil, err
	}
	return projectID, nil
}

// GetTaskProjectIDDeletedToo is GetTaskProjectID for a task that may have
// been deleted: the project it is, or was, in.
func GetTaskProjectIDDeletedToo(ctx context.Context, taskUUID uuid.UUID) (uuid.UUID, error) {
	projectID, err := models.GetTaskProjectID(ctx, taskUUID, true)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTaskProjectIDDeletedToo Failed err: %+v", err)
		return uuid.Nil, err
	}
	return projectID, nil
}

// SetGitHubSyncStatus updates the sync status for a task.
func SetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID, status string, errMsg *string, attempts int) error {
	err := models.SetGitHubSyncStatus(ctx, taskUUID, status, errMsg, attempts)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetGitHubSyncStatus Failed err: %+v", err)
		return err
	}
	return nil
}

// GetGitHubSyncStatus returns the sync status for a task.
func GetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID) (status string, errMsg *string, attempts int, err error) {
	status, errMsg, attempts, err = models.GetGitHubSyncStatus(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubSyncStatus Failed err: %+v", err)
		return "", nil, 0, err
	}
	return
}

// GetGitHubMetaForTask returns all GitHub metadata for a task from PostgreSQL.
func GetGitHubMetaForTask(ctx context.Context, taskUUID uuid.UUID) (*models.GitHubMeta, error) {
	meta, err := models.GetGitHubMetaForTask(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubMetaForTask Failed err: %+v", err)
		return nil, err
	}
	return meta, nil
}

// GetGitHubMetaForTasks returns GitHub metadata for multiple tasks in one query.
func GetGitHubMetaForTasks(ctx context.Context, taskUUIDs []uuid.UUID) (map[uuid.UUID]*models.GitHubMeta, error) {
	meta, err := models.GetGitHubMetaForTasks(ctx, taskUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetGitHubMetaForTasks Failed err: %+v", err)
		return nil, err
	}
	return meta, nil
}

// BulkArchiveTasksInDgraph sets task_deleted_at on multiple tasks in a single Dgraph mutation (batched).
// ClosedTaskUUIDsBefore answers up to limit live tasks that are done or
// canceled, were created before cutoff, and have been closed since before it
// (a task closed before the status date was recorded counts as closed since
// it was created): what archiving completed tasks may move. A task's status
// is kept here, in the graph, not in Postgres.
func ClosedTaskUUIDsBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	query := fmt.Sprintf(`query ClosedTasks($cutoff: string) {
		tasks(func: eq(task_status, ["%s", "%s"]), first: %d) @filter(%s AND lt(task_created_at, $cutoff) AND (not has(task_status_since) OR lt(task_status_since, $cutoff))) {
			task_uuid
		}
	}`, dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_CANCELED, limit, dgraphStruct.TASK_LIVE_FILTER)
	tasks, err := dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$cutoff": cutoff.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	uuids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		uuids = append(uuids, t.Uuid)
	}
	return uuids, nil
}

func BulkArchiveTasksInDgraph(ctx context.Context, taskUUIDs []string) error {
	if len(taskUUIDs) == 0 {
		return nil
	}
	now := time.Now()
	const batchSize = 500
	for start := 0; start < len(taskUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(taskUUIDs) {
			end = len(taskUUIDs)
		}
		batch := taskUUIDs[start:end]
		query := "query {\n"
		tasks := make([]*dgraphStruct.DgraphTask, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  ta%d as var(func: eq(task_uuid, \"%s\"))\n", i, uuidStr)
			tasks[i] = &dgraphStruct.DgraphTask{
				DType:     []string{"Task"},
				Uid:       fmt.Sprintf("uid(ta%d)", i),
				DeletedAt: &now,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphTasks(ctx, tasks, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkArchiveTasksInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// BulkRestoreTasksInDgraph clears task_deleted_at on multiple tasks in a single Dgraph mutation (batched).
func BulkRestoreTasksInDgraph(ctx context.Context, taskUUIDs []string) error {
	if len(taskUUIDs) == 0 {
		return nil
	}
	zeroTime := time.Time{}
	const batchSize = 500
	for start := 0; start < len(taskUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(taskUUIDs) {
			end = len(taskUUIDs)
		}
		batch := taskUUIDs[start:end]
		query := "query {\n"
		tasks := make([]*dgraphStruct.DgraphTask, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  ta%d as var(func: eq(task_uuid, \"%s\"))\n", i, uuidStr)
			tasks[i] = &dgraphStruct.DgraphTask{
				DType:     []string{"Task"},
				Uid:       fmt.Sprintf("uid(ta%d)", i),
				DeletedAt: &zeroTime,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphTasks(ctx, tasks, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkRestoreTasksInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// GetDgraphTaskRanks returns the named tasks with what their kanban order is
// computed from: rank and creation time. Unknown uuids are left out.
func GetDgraphTaskRanks(ctx context.Context, taskUUIDs []string) (tasks []*dgraphStruct.DgraphTask, err error) {
	if len(taskUUIDs) == 0 {
		return nil, nil
	}
	ids, err := json.Marshal(taskUUIDs)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`{
				tasks(func: eq(task_uuid, %s)) @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) {
					task_uuid
					task_rank
					task_created_at
				}
			}`, ids)
	return dgraphModels.QueryDgraphTasks(ctx, query, nil)
}

// GetDgraphProjectRanks is every live task of a project with its rank, for
// renumbering the project's one order (business/Task Reorder). Unsorted, so
// Dgraph sends them all; the order is business/TaskRank's.
func GetDgraphProjectRanks(ctx context.Context, projectUUID string) ([]*dgraphStruct.DgraphTask, error) {
	query := `query Ranks($project: string){
				project(func: eq(project_uuid, $project)) {
					tasks: project_tasks @filter(` + dgraphquery.Live("task_deleted_at") + `) {
						task_uuid
						task_rank
						task_created_at
					}
				}
			}`
	return dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$project": projectUUID})
}

// SetDgraphTaskRanks writes each task's rank in one transaction.
func SetDgraphTaskRanks(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	return writeDgraphTasks(ctx, tasks, func(from, to *dgraphStruct.DgraphTask) { to.Rank = from.Rank })
}

// SetDgraphTaskCreatedAt writes when each task was made, in one transaction.
func SetDgraphTaskCreatedAt(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	return writeDgraphTasks(ctx, tasks, func(from, to *dgraphStruct.DgraphTask) { to.CreatedAt = from.CreatedAt })
}

// SetDgraphTaskStatusSince writes when each task entered its status, in one transaction.
func SetDgraphTaskStatusSince(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	return writeDgraphTasks(ctx, tasks, func(from, to *dgraphStruct.DgraphTask) { to.StatusSince = from.StatusSince })
}

// writeDgraphTasks writes, for each task (found by its uuid), the fields fill
// copies over, all in one transaction.
func writeDgraphTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask, fill func(from, to *dgraphStruct.DgraphTask)) error {
	if len(tasks) == 0 {
		return nil
	}
	query := "query {\n"
	writes := make([]*dgraphStruct.DgraphTask, len(tasks))
	for i, t := range tasks {
		if _, err := uuid.Parse(t.Uuid); err != nil {
			return fmt.Errorf("task %q: %w", t.Uuid, err)
		}
		query += fmt.Sprintf("  tr%d as var(func: eq(task_uuid, %q))\n", i, t.Uuid)
		writes[i] = &dgraphStruct.DgraphTask{Uid: fmt.Sprintf("uid(tr%d)", i)}
		fill(t, writes[i])
	}
	query += "}"
	return dgraphModels.BulkSoftDeleteDgraphTasks(ctx, writes, query)
}

// GetDgraphTasksWithoutStatusSince returns up to n tasks that do not yet say
// when they entered their status, with what is needed to work it out.
func GetDgraphTasksWithoutStatusSince(ctx context.Context, n int) ([]*dgraphStruct.DgraphTask, error) {
	query := fmt.Sprintf(`{
				tasks(func: has(task_uuid), first: %d) @filter(NOT has(task_status_since)) {
					task_uuid
					task_created_at
					task_activities {
						activity_type
						activity_time
					}
				}
			}`, n)
	// The backfill writes every task's time, restricted ones too, and shows
	// nothing to anyone (models/taskread).
	return dgraphModels.QueryDgraphTasks(taskread.WithSystem(ctx), query, nil)
}

// clearJSON is a Dgraph delete for the named predicates of the nodes in var v.
func clearJSON(v string, predicates []string) (string, error) {
	if len(predicates) == 0 {
		return "", nil
	}
	del := map[string]interface{}{"uid": "uid(" + v + ")"}
	for _, p := range predicates {
		del[p] = nil
	}
	b, err := json.Marshal(del)
	return string(b), err
}

// UpdateDgraphTaskClearing writes dgraphTask (its Uid must be "uid(task)") and,
// in the same transaction, removes the named predicates from it, such as a
// custom status that no longer applies. Setting them to "" instead would leave
// a value behind that has() still sees.
func UpdateDgraphTaskClearing(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask, clear []string) error {
	dgraphTask.DType = []string{"Task"}
	del, err := clearJSON("task", clear)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`query { task as var(func: eq(task_uuid, %q)) }`, dgraphTask.Uuid)
	err = dgraphModels.UpdateExistingTasks(ctx, dgraphTask, query, del)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateDgraphTaskClearing err: %+v", err)
	}
	return err
}

// RestatusTasksInCustomStatus moves every task in the custom status fromID to
// the given category and custom status (both "" clear it), in one transaction.
// Used when a custom status is renamed, recategorised or deleted.
func RestatusTasksInCustomStatus(ctx context.Context, fromID string, category string, customID, customName string) error {
	if _, err := uuid.Parse(fromID); err != nil {
		return fmt.Errorf("custom status id %q: %w", fromID, err)
	}
	set := &dgraphStruct.DgraphTask{Uid: "uid(task)", Status: category}
	var clear []string
	if customID != "" {
		set.CustomStatus, set.CustomStatusName = &customID, &customName
	} else {
		clear = []string{"task_custom_status", "task_custom_status_name"}
	}
	del, err := clearJSON("task", clear)
	if err != nil {
		return err
	}
	set.DType = []string{"Task"}
	query := fmt.Sprintf(`query {
		task as var(func: eq(task_custom_status, %q))
		affected(func: uid(task)) { task_uuid }
	}`, fromID)
	uuids, err := dgraphModels.UpdateExistingTasksReturning(ctx, set, query, del)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RestatusTasksInCustomStatus err: %+v", err)
		return err
	}
	// Search keeps each task's category. It changes when the status moves to
	// another one, or its tasks move on its deletion.
	helpers.GoSafeNamed("syncTaskStatusToOpenSearch", func() { syncTaskStatusToOpenSearch(uuids, category) })
	return nil
}

// syncTaskStatusToOpenSearch sets the category on each task's search document.
// Best effort, as every other search write here: the graph is the record.
func syncTaskStatusToOpenSearch(uuids []string, category string) {
	now := time.Now().Unix()
	docs := make([]*openSearchStruct.OpenSearchTask, 0, len(uuids))
	for _, id := range uuids {
		docs = append(docs, &openSearchStruct.OpenSearchTask{Uuid: id, TaskStatus: category, TaskUpdatedAt: now})
	}
	// One bulk request for them all (PR2, the BE audit's #32), not one each.
	UpdateTasksInOpenSearch(docs)
}

// UpdateTasksInOpenSearch updates many tasks' search documents in one bulk
// request. Best effort, as every search write here.
func UpdateTasksInOpenSearch(docs []*openSearchStruct.OpenSearchTask) {
	ctx := context.Background()
	if err := OpenSearchModels.UpdateTasksInOpenSearch(ctx, docs); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateTasksInOpenSearch couldn't update %d tasks err: %+v", len(docs), err)
	}
}

// GetDgraphTaskStatuses is the status of each live task listed, when it was
// set and the task's estimate, for a cycle's progress, burndown and velocity.
// Archived tasks are left out.
func GetDgraphTaskStatuses(ctx context.Context, taskUUIDs []string) (tasks []*dgraphStruct.DgraphTask, err error) {
	if len(taskUUIDs) == 0 {
		return nil, nil
	}
	ids, err := json.Marshal(taskUUIDs)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`{
				tasks(func: eq(task_uuid, %s)) @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) {
					task_uuid
					task_status
					task_status_since
					task_estimate_minutes
				}
			}`, ids)
	return dgraphModels.QueryDgraphTasks(ctx, query, nil)
}

// GetDgraphTaskForGuest is what a project's guest may see of one task: its
// card, its description and its comments with their authors' names. Nothing
// about the viewer is asked, because a guest is not a user; the caller has
// already checked the task belongs to the guest's project.
func GetDgraphTaskForGuest(ctx context.Context, taskUUID string) (*dgraphStruct.DgraphTask, error) {
	query := `query TaskInfo($id: string){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid
					task_name
					task_status
					task_custom_status_name
					task_priority
					task_description
					task_due_date
					task_start_date
					task_created_at
					task_deleted_at
					task_assignee { user_name user_full_name }
					task_project { project_uuid }
					task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
						comment_text
						comment_created_at
						comment_by { user_name user_full_name is_bot }
					}
				}
			}`
	t, err := dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, map[string]string{"$id": taskUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphTaskForGuest err: %+v", err)
		return t, err
	}
	// Oldest first, sorted here: Dgraph stops a sorted block at 1,000. As
	// Dgraph did, a comment with no time goes last.
	slices.SortStableFunc(t.Comments, func(a, b *dgraphStruct.DgraphComment) int {
		return helpers.CompareTimes(a.CreatedAt, b.CreatedAt, false)
	})
	return t, nil
}

// GetDgraphProjectTaskLabels returns the labels (tag lists) of a project's
// live tasks that have one. task_uuid is asked for because QueryDgraphTasks
// keeps only rows that carry one.
func GetDgraphProjectTaskLabels(ctx context.Context, projectUUID string) ([]*dgraphStruct.DgraphTask, error) {
	query := `query Labels($project: string){
				var(func: eq(project_uuid, $project)) {
					t as project_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND has(task_label))
				}
				tasks(func: uid(t)) {
					task_uuid
					task_label
				}
			}`
	return dgraphModels.QueryDgraphTasks(ctx, query, map[string]string{"$project": projectUUID})
}
