package business

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	syncBusiness "github.com/akashc777/OneCamp/business"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	businessAttachment "github.com/akashc777/OneCamp/business/Attachment"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	userProjectNotificationBusiness "github.com/akashc777/OneCamp/business/UserProjectNotification"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	githubCommentMappingDomain "github.com/akashc777/OneCamp/domain/GitHubCommentMapping"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func CreateTaskComment(ctx context.Context, taskUUID uuid.UUID, rawTaskDgraph *dgraphStruct.DgraphTask, createdByUser *model.UserInfo, createTaskCommentInfoInput *adapter.CreateOrUpdateTaskCommentInput, mentionsDgraphUsersList []*dgraphStruct.DgraphUser) (commentInfoRes *adapter.OutputCreateCommentForTask, err error) {

	commentUUID := uuid.New()
	currentTime := time.Now()
	if createTaskCommentInfoInput.CreatedAt != nil {
		currentTime = *createTaskCommentInfoInput.CreatedAt
	}
	zeroUnixTime := time.Time{}

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	for _, mediaObj := range createTaskCommentInfoInput.Attachments {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: createdByUser.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(ta)",
		Uuid: rawTaskDgraph.Uuid,
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  commentUUID.String(),
				Text:  createTaskCommentInfoInput.CommentBody,
				Task: &dgraphStruct.DgraphTask{
					Uid: "uid(ta)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: createdByUser.UserDgraphInfo.Uid,
				},
				Attachments: createTaskCommentInfoInput.Attachments,
				CreatedAt:   &currentTime,
				DeletedAt:   &zeroUnixTime,
				Mentions: &dgraphStruct.DgraphMentions{
					CreatedAt:   &currentTime,
					Mentions:    commentMentions,
					CommentUuid: commentUUID.String(),
					Comment: &dgraphStruct.DgraphComment{
						Uid: "uid(co)",
					},
					DType: []string{"Mention"},
				},
				CommentBy: &dgraphStruct.DgraphUser{
					Uid: createdByUser.UserDgraphInfo.Uid,
				},
			},
		},
	}

	_, err = businessComment.CreateCommentInTask(ctx, dgraphTask, createdByUser, commentUUID, currentTime, rawTaskDgraph, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateTaskComment Failed to create comment in dgraph err: %+v", err)
		return
	}

	taskCommentMqttStruct := &mqttStruct.MqttTaskComment{
		Type:           mqttStruct.TYPE_CREATE,
		CommentUuid:    commentUUID.String(),
		TaskUuid:       rawTaskDgraph.Uuid,
		UserUuid:       createdByUser.UserDgraphInfo.Uuid,
		UserName:       createdByUser.UserDgraphInfo.UserName,
		UserProfileKey: createdByUser.UserDgraphInfo.ProfileKey,
		CreatedAt:      &currentTime,
		HTMLText:       createTaskCommentInfoInput.CommentBody,
		Attachments:    createTaskCommentInfoInput.Attachments,
	}

	go mqttBusiness.PublishTaskComment(taskCommentMqttStruct, rawTaskDgraph.Project.Uuid)

	commentInfoRes = &adapter.OutputCreateCommentForTask{
		Uuid:             commentUUID.String(),
		CommentCreatedAt: currentTime,
	}

	go publishTaskCommentActivity(createTaskCommentInfoInput.CommentBody, &currentTime, mentionsDgraphUsersList, commentUUID.String(), rawTaskDgraph, &createdByUser.UserDgraphInfo)

	// Notify listeners (an AI teammate working this task can resume/continue on
	// a human reply). Loop-safe: a worker posting an agent's own status comment
	// tags the context workflow-generated, so the event bus skips listeners for
	// it. The plain-text body is sent so the agent reads what was said.
	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.comment.created", map[string]interface{}{
		"task_id":   rawTaskDgraph.Uuid,
		"author_id": createdByUser.UserDgraphInfo.Uuid,
		"body":      helpers.HTMLToPlainText(createTaskCommentInfoInput.CommentBody),
	})

	if !createTaskCommentInfoInput.SkipGitHubSync {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "comment", map[string]interface{}{
			"comment_body": createTaskCommentInfoInput.CommentBody,
			"comment_uuid": commentUUID.String(),
		})
	}

	return
}

func publishTaskCommentActivity(commentBody string, currentTime *time.Time, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, commentUUID string, rawTaskDgraph *dgraphStruct.DgraphTask, createdByUserDgraph *dgraphStruct.DgraphUser) {
	ctx := context.Background()
	mentionUUIDs := []string{}
	for _, m := range mentionsDgraphUsersList {
		mentionUUIDs = append(mentionUUIDs, m.Uuid)
	}

	eligibleMentionsUserIDs, err := userProjectNotificationBusiness.GetEligibleUsersForProjectActivity(ctx, rawTaskDgraph.Project.Uuid, createdByUserDgraph.Uuid, mentionUUIDs, true)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/publishTaskCommentActivity Failed to get users uuids err: %+v", err)
		return
	}

	for _, recipientID := range eligibleMentionsUserIDs {
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid: commentUUID,
				Text: commentBody,
				CommentBy: &dgraphStruct.DgraphUser{
					Uuid:     createdByUserDgraph.Uuid,
					UserName: createdByUserDgraph.UserName,
				},
				Task: &dgraphStruct.DgraphTask{
					Uuid: rawTaskDgraph.Uuid,
					Name: rawTaskDgraph.Name,
					Project: &dgraphStruct.DgraphProject{
						Uuid: rawTaskDgraph.Project.Uuid,
					},
				},
				CreatedAt: currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(recipientID, activityItem)
	}

	assigneeCreatedUserUID := ""
	if rawTaskDgraph.CreatedBy != nil {
		assigneeCreatedUserUID = rawTaskDgraph.CreatedBy.Uid
	}
	if rawTaskDgraph.Assignee != nil {
		assigneeCreatedUserUID = rawTaskDgraph.Assignee.Uuid
	}

	activityItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
		Time:         time.Now().Format(time.RFC3339),
		Comment: &dgraphStruct.DgraphComment{
			Uuid: commentUUID,
			Text: commentBody,
			CommentBy: &dgraphStruct.DgraphUser{
				Uuid:     createdByUserDgraph.Uuid,
				UserName: createdByUserDgraph.UserName,
			},
			Task: &dgraphStruct.DgraphTask{
				Uuid: rawTaskDgraph.Uuid,
				Name: rawTaskDgraph.Name,
				Project: &dgraphStruct.DgraphProject{
					Uuid: rawTaskDgraph.Project.Uuid,
				},
			},
			CreatedAt: currentTime,
		},
	}
	activityBusiness.PublishActivityToUser(assigneeCreatedUserUID, activityItem)
}

func UpdateTaskCommentBody(ctx context.Context, commentUUID uuid.UUID, createTaskCommentInfoInput *adapter.CreateOrUpdateTaskCommentInput, rawDgraphCommentInfo *dgraphStruct.DgraphComment, mentionsDgraphUsersList []*dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	dgraphComment := dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  createTaskCommentInfoInput.Uuid,
		DType: []string{"Comment"},
		Text:  createTaskCommentInfoInput.CommentBody,
		// Attachments: commentInfo.MediaObj,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid: "uid(me)",
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
			CommentUuid: createTaskCommentInfoInput.Uuid,
			Mentions:    commentMentions,
		},
		UpdatedAt: &currentTime,
	}

	err = businessComment.UpdateComment(ctx, &dgraphComment, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskCommentBody Failed to update comment in dgraph err: %+v", err)
		return
	}

	taskCommentMqttStruct := &mqttStruct.MqttTaskComment{
		Type:        mqttStruct.TYPE_UPDATE,
		CommentUuid: commentUUID.String(),
		TaskUuid:    createTaskCommentInfoInput.TaskUuid,
		UpdatedAt:   &currentTime,
		HTMLText:    createTaskCommentInfoInput.CommentBody,
	}

	go mqttBusiness.PublishTaskComment(taskCommentMqttStruct, rawDgraphCommentInfo.Task.Project.Uuid)

	// Sync comment edit to GitHub if this comment was originally synced from GitHub.
	if !createTaskCommentInfoInput.SkipGitHubSync && rawDgraphCommentInfo.Task != nil && rawDgraphCommentInfo.Task.Uuid != "" {
		if taskUUID, parseErr := uuid.Parse(rawDgraphCommentInfo.Task.Uuid); parseErr == nil {
			if ghID, _, _, lookupErr := githubCommentMappingDomain.GetGitHubCommentIDByCommentUUID(ctx, commentUUID); lookupErr == nil && ghID > 0 {
				payload := map[string]interface{}{
					"github_comment_id": ghID,
					"comment_body":      createTaskCommentInfoInput.CommentBody,
				}
				go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "comment_edit", payload)
			}
		}
	}

	return
}

func ArchiveCommentByCommentUUID(ctx context.Context, commentUUID uuid.UUID, rawDgraphComment *dgraphStruct.DgraphComment) (err error) {

	currentTime := time.Now()
	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:       "uid(co)",
		Uuid:      commentUUID.String(),
		DeletedAt: &currentTime,
		DType:     []string{"Comment"},
	}
	err = businessComment.DeleteComment(ctx, dgraphComment, currentTime, commentUUID, rawDgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/ArchiveCommentByCommentUUID Failed to delete comment from post err: %+v",
			err)

		return
	}

	taskCommentMqttStruct := &mqttStruct.MqttTaskComment{
		Type:        mqttStruct.TYPE_DELETE,
		CommentUuid: commentUUID.String(),
		UpdatedAt:   &currentTime,
	}

	go mqttBusiness.PublishTaskComment(taskCommentMqttStruct, rawDgraphComment.Task.Project.Uuid)

	// Sync comment delete to GitHub if this comment was originally synced from GitHub.
	if rawDgraphComment.Task != nil && rawDgraphComment.Task.Uuid != "" {
		if taskUUID, parseErr := uuid.Parse(rawDgraphComment.Task.Uuid); parseErr == nil {
			if ghID, _, _, lookupErr := githubCommentMappingDomain.GetGitHubCommentIDByCommentUUID(ctx, commentUUID); lookupErr == nil && ghID > 0 {
				payload := map[string]interface{}{
					"github_comment_id": ghID,
				}
				go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "comment_delete", payload)
			}
		}
	}

	return

}

func CreateSubTask(ctx context.Context, projectUUID uuid.UUID, userInfo *model.UserInfo, projectDgraphInfo *dgraphStruct.DgraphProject, assigneeDgraphInfo *dgraphStruct.DgraphUser, taskInfo adapter.CreateOrUpdateTaskInput) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	currentTime := time.Now()
	taskUUID := uuid.New()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.CreateTask(ctx, taskUUID, projectUUID, userInfo.UserPostgresInfo.Id, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateSubTask failed to create new task in postgres err: %+v", err)
		return
	}

	dgraphTask = &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Project: &dgraphStruct.DgraphProject{
			Uid: projectDgraphInfo.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		},
		ParentTask: &dgraphStruct.DgraphTask{
			Uid: projectDgraphInfo.Tasks[0].Uid,
			SubTasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
			Activity: []*dgraphStruct.DgraphTaskActivity{{
				DType: []string{"activity"},
				Uuid:  activityUUID.String(),
				CreatedBy: &dgraphStruct.DgraphUser{
					Uid: userInfo.UserDgraphInfo.Uid,
				},
				Type:      dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK,
				NextState: taskInfo.TaskName,
				LogTime:   &currentTime,
			}},
		},
		Team: &dgraphStruct.DgraphTeam{
			Uid: projectDgraphInfo.Team.Uid,
		},
		StartDate: &zeroUnixTime,
		DueDate:   &zeroUnixTime,
		Name:      taskInfo.TaskName,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		},
		Priority:  dgraphStruct.TASK_PRIORITY_MEDIUM,
		Status:    dgraphStruct.TASK_STATUS_TODO,
		CreatedAt: &currentTime,
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	if len(taskInfo.DueDate) > 0 {
		var dueDate time.Time
		dueDate, err = time.Parse(time.RFC3339, taskInfo.DueDate)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CreateSubTask Failed parse due date err: %+v", err)
			return
		}
		dgraphTask.DueDate = &dueDate
	}

	if len(taskInfo.StartDate) > 0 {
		var startDate time.Time
		startDate, err = time.Parse(time.RFC3339, taskInfo.StartDate)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CreateSubTask Failed parse start date err: %+v", err)
			return
		}
		dgraphTask.StartDate = &startDate
	}

	emptyString := ""

	opensearchTask := &openSearchStruct.OpenSearchTask{
		Uuid:                  dgraphTask.Uuid,
		TaskName:              dgraphTask.Name,
		TaskDescription:       dgraphTask.Description,
		TaskAssigneeUuid:      &emptyString,
		TaskCreatedAt:         currentTime.Unix(),
		TaskDeletedAt:         nil,
		TaskProjectUuid:       projectDgraphInfo.Uuid,
		TaskProjectName:       projectDgraphInfo.Name,
		TaskStatus:            dgraphTask.Status,
		TaskPriority:          dgraphTask.Priority,
		TaskLabel:             dgraphTask.Label,
		TaskCreatedByUserUuid: userInfo.UserDgraphInfo.Uuid,
	}

	if assigneeDgraphInfo != nil {
		dgraphTask.Assignee = &dgraphStruct.DgraphUser{
			Uid: assigneeDgraphInfo.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		}
		opensearchTask.TaskAssigneeUuid = &assigneeDgraphInfo.Uuid
		opensearchTask.TaskAssigneeFullName = assigneeDgraphInfo.UserFullName
	}

	taskUID, err := domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if helpers.DgraphWriteFailed(taskUID, err) {
		helpers.LogErrorWithContext(ctx, "business/CreateSubTask failed to create new sub task in dgraph err: %+v", err)
		return
	}

	var emptyAttachments []*dgraphStruct.DgraphAttachment

	go domain.CreateTaskWithAttachmentsInOpensearch(opensearchTask, emptyAttachments, &userInfo.UserDgraphInfo)

	// embed for AI Second Brain (async)
	assigneeUUIDStr := ""
	if assigneeDgraphInfo != nil {
		assigneeUUIDStr = assigneeDgraphInfo.Uuid
	}
	ai.EmbedTaskContent(taskInfo.TaskName, "", taskUUID.String(), projectDgraphInfo.Uuid, projectDgraphInfo.Team.Uuid, assigneeUUIDStr, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.UserName)

	if assigneeDgraphInfo != nil {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), assigneeDgraphInfo.Uuid)
	}

	return
}

func RemoveAttachmentFromTask(ctx context.Context, attachmentUUID uuid.UUID, dgraphTaskUID string, userDgraphUID string) (err error) {
	err = businessAttachment.ArchiveAttachmentByAttachmentUUID(ctx, attachmentUUID, dgraphTaskUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAttachmentFromTask failed to archive attachment err: %+v", err)
		return
	}

	return

}

func AddAttachmentToTask(ctx context.Context, taskUUID uuid.UUID, dgraphTaskInfo *dgraphStruct.DgraphTask, taskInfo *adapter.CreateOrUpdateTaskInput, userInfo *dgraphStruct.DgraphUser) (err error) {

	zeroUnixTime := time.Time{}
	currentTime := time.Now()
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAttachmentToTask failed to update task in postgres err: %+v", err)
		return
	}

	var newAttachmentsName []string

	var oldAttachmentsName []string

	for _, attachment := range taskInfo.Attachments {
		newAttachmentsName = append(newAttachmentsName, attachment.FileName)
		attachment.DType = []string{"Attachment"}
		attachment.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.Uid,
		}
		attachment.CreatedAt = &currentTime
	}

	for _, attachment := range dgraphTaskInfo.Attachments {
		oldAttachmentsName = append(oldAttachmentsName, attachment.FileName)
	}

	newState := strings.Join(append(newAttachmentsName, oldAttachmentsName...), ",")
	oldState := strings.Join(oldAttachmentsName, ",")

	dgraphTask := dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Project: &dgraphStruct.DgraphProject{
			Uid: dgraphTaskInfo.Project.Uid,
		},
		Attachments: taskInfo.Attachments,
		DeletedAt:   &zeroUnixTime,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid:      activityUUID.String(),
			Type:      dgraphStruct.ACTIVITY_TYPE_ADD_ATACHMENT,
			NextState: newState,
			PrevState: oldState,
			LogTime:   &currentTime,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
		}},
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, &dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AddAttachmentToTask failed to update task in dgraph err: %+v", err)
		return
	}

	go domain.CreateTaskAttachmentsInOpensearch(&currentTime, dgraphTaskInfo, taskInfo.Attachments, userInfo)

	return

}

func CreateTask(ctx context.Context, projectUUID uuid.UUID, userInfo *model.UserInfo, projectDgraphInfo *dgraphStruct.DgraphProject, assigneeDgraphInfo *dgraphStruct.DgraphUser, taskInfo adapter.CreateOrUpdateTaskInput, mentionsUsers []*dgraphStruct.DgraphUser) (taskUUIDRes uuid.UUID, err error) {

	// A built-in status or one of the project's own; none given is Todo.
	status, err := taskStatusBusiness.Resolve(ctx, projectUUID.String(), taskInfo.Status)
	if err != nil {
		return
	}

	taskUUID := uuid.New()
	zeroUnixTime := time.Time{}
	currentTime := time.Now()
	activityUUID := uuid.New()

	err = domain.CreateTask(ctx, taskUUID, projectUUID, userInfo.UserPostgresInfo.Id, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/createTask failed to create new task in postgres err: %+v", err)
		return
	}

	for _, mediaObj := range taskInfo.Attachments {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphTask := dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Project: &dgraphStruct.DgraphProject{
			Uid: projectDgraphInfo.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		},
		Team: &dgraphStruct.DgraphTeam{
			Uid: projectDgraphInfo.Team.Uid,
		},
		Mentions: &dgraphStruct.DgraphMentions{
			CreatedAt: &currentTime,
			Mentions:  mentionsUsers,
			TaskUuid:  taskUUID.String(),
			Task: &dgraphStruct.DgraphTask{
				Uid: "uid(task)",
			},
			DType: []string{"Mention"},
		},
		StartDate:   &zeroUnixTime,
		DueDate:     &zeroUnixTime,
		Name:        taskInfo.TaskName,
		Attachments: taskInfo.Attachments,
		Description: &taskInfo.TaskDescription,
		Status:      status.Category,
		Priority:    taskInfo.Priority,
		Label:       &taskInfo.Label,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		},
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			DType: []string{"activity"},
			Uuid:  activityUUID.String(),
			Type:  dgraphStruct.ACTIVITY_TYPE_CREATE_TASK,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.UserDgraphInfo.Uid,
			},
			LogTime: &currentTime,
		}},
		CreatedAt: &currentTime,
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}
	if status.CustomID != "" {
		dgraphTask.CustomStatus, dgraphTask.CustomStatusName = &status.CustomID, &status.CustomName
	}

	if len(taskInfo.DueDate) > 0 {
		var dueDate time.Time
		dueDate, err = time.Parse(time.RFC3339, taskInfo.DueDate)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/createTask Failed parse due date err: %+v", err)
			return
		}
		dgraphTask.DueDate = &dueDate
	}

	if len(taskInfo.StartDate) > 0 {
		var startDate time.Time
		startDate, err = time.Parse(time.RFC3339, taskInfo.StartDate)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/createTask Failed parse start date err: %+v", err)
			return
		}
		dgraphTask.StartDate = &startDate
	}

	emptyString := ""
	opensearchTask := &openSearchStruct.OpenSearchTask{
		Uuid:                  dgraphTask.Uuid,
		TaskName:              dgraphTask.Name,
		TaskDescription:       dgraphTask.Description,
		TaskAssigneeUuid:      &emptyString,
		TaskCreatedAt:         currentTime.Unix(),
		TaskDeletedAt:         nil,
		TaskProjectUuid:       projectDgraphInfo.Uuid,
		TaskProjectName:       projectDgraphInfo.Name,
		TaskStatus:            dgraphTask.Status,
		TaskPriority:          dgraphTask.Priority,
		TaskLabel:             dgraphTask.Label,
		TaskCreatedByUserUuid: userInfo.UserDgraphInfo.Uuid,
	}

	if assigneeDgraphInfo != nil {
		dgraphTask.Assignee = &dgraphStruct.DgraphUser{
			Uid: assigneeDgraphInfo.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		}
		opensearchTask.TaskAssigneeUuid = &assigneeDgraphInfo.Uuid
		opensearchTask.TaskAssigneeFullName = assigneeDgraphInfo.UserFullName
	}

	taskUID, err := domain.CreateOrUpdateDgraphTask(ctx, &dgraphTask)

	if helpers.DgraphWriteFailed(taskUID, err) {
		helpers.LogErrorWithContext(ctx, "business/createTask failed to create new task in dgraph err: %+v", err)
		return
	}

	go domain.CreateTaskWithAttachmentsInOpensearch(opensearchTask, taskInfo.Attachments, &userInfo.UserDgraphInfo)

	// embed for AI Second Brain (async)
	assigneeUUIDStr := ""
	if assigneeDgraphInfo != nil {
		assigneeUUIDStr = assigneeDgraphInfo.Uuid
	}
	desc := ""
	if taskInfo.TaskDescription != "" {
		desc = taskInfo.TaskDescription
	}
	ai.EmbedTaskContent(taskInfo.TaskName, desc, taskUUID.String(), projectDgraphInfo.Uuid, projectDgraphInfo.Team.Uuid, assigneeUUIDStr, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.UserName)

	go sendNewTaskNotification(taskInfo.TaskDescription, projectDgraphInfo.Uuid, taskInfo.TaskName, mentionsUsers, taskUUID.String(), userInfo.UserDgraphInfo, assigneeDgraphInfo)

	if assigneeDgraphInfo != nil {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), assigneeDgraphInfo.Uuid)
	}

	// If GitHub issue/PR URL provided at creation, persist it in both PG and Dgraph
	if taskInfo.GitHubIssueURL != nil && *taskInfo.GitHubIssueURL != "" {
		if taskInfo.GitHubIssueNum != nil {
			domain.SetGitHubIssueFieldsOnTask(ctx, taskUUID, *taskInfo.GitHubIssueNum, *taskInfo.GitHubIssueURL)
		}
		dgTask := &dgraphStruct.DgraphTask{
			Uid:               "uid(task)",
			Uuid:              taskUUID.String(),
			GitHubIssueNumber: taskInfo.GitHubIssueNum,
			GitHubIssueURL:    taskInfo.GitHubIssueURL,
		}
		domain.CreateOrUpdateDgraphTask(ctx, dgTask)
	}
	if taskInfo.GitHubPRURL != nil && *taskInfo.GitHubPRURL != "" {
		if taskInfo.GitHubPRNum != nil {
			domain.SetGitHubPRFieldsOnTask(ctx, taskUUID, *taskInfo.GitHubPRNum, *taskInfo.GitHubPRURL, "")
		}
		dgTask := &dgraphStruct.DgraphTask{
			Uid:            "uid(task)",
			Uuid:           taskUUID.String(),
			GitHubPRNumber: taskInfo.GitHubPRNum,
			GitHubPRURL:    taskInfo.GitHubPRURL,
		}
		domain.CreateOrUpdateDgraphTask(ctx, dgTask)
	}

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.created", map[string]interface{}{
		"task_id":     taskUUID.String(),
		"project_id":  projectUUID.String(),
		"name":        taskInfo.TaskName,
		"description": taskInfo.TaskDescription,
		"status":      taskInfo.Status,
		"priority":    taskInfo.Priority,
		"created_by":  userInfo.UserDgraphInfo.Uuid,
	})

	// If the task was created already assigned to an AI teammate, hand it off
	// immediately (same durable path as a later re-assignment).
	if assigneeDgraphInfo != nil && assigneeDgraphInfo.Uuid != "" {
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.assigned", map[string]interface{}{
			"task_id":          taskUUID.String(),
			"project_id":       projectUUID.String(),
			"name":             taskInfo.TaskName,
			"description":      taskInfo.TaskDescription,
			"assignee_id":      assigneeDgraphInfo.Uuid,
			"assigned_by_name": userInfo.UserDgraphInfo.UserName,
			"assigned_by":      userInfo.UserDgraphInfo.Uuid,
		})
	}

	taskUUIDRes = taskUUID
	return
}

func sendNewTaskNotification(body string, projectId string, taskName string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, taskUUID string, userDgraph dgraphStruct.DgraphUser, assigneeDrgraph *dgraphStruct.DgraphUser) {

	mentionsUUIDList := []string{}
	mentionsMap := make(map[string]bool)

	ctx := context.Background()

	for _, mention := range mentionsDgraphUsersList {
		mentionsUUIDList = append(mentionsUUIDList, mention.Uuid)
		mentionsMap[mention.Uuid] = true
	}

	// 1. Get eligible users for notifications (based on preferences)
	eligibleUserIDs, err := userProjectNotificationBusiness.GetEligibleUsersForProjectActivity(ctx, projectId, userDgraph.Uuid, mentionsUUIDList, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewTaskNotification Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Publish MQTT Activity Events
	// Filter eligibleUserIDs to find those who are also in mentionsUUIDList
	eligibleMentionsUserIDs := []string{}

	for _, uid := range eligibleUserIDs {
		if mentionsMap[uid] {
			eligibleMentionsUserIDs = append(eligibleMentionsUserIDs, uid)
		}
	}

	for _, recipientID := range eligibleMentionsUserIDs {

		if recipientID == userDgraph.Uuid {
			continue
		}
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In channels, new posts are notifications for members
			Time:         time.Now().Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				TaskUuid: taskUUID,
				Task: &dgraphStruct.DgraphTask{
					Uuid: taskUUID,
					Name: taskName,
					Project: &dgraphStruct.DgraphProject{
						Uuid: projectId,
					},
				},

				CreatedAt: helpers.TimePointer(time.Now()),
			},
		}
		activityBusiness.PublishActivityToUser(recipientID, activityItem)
	}

	// 3. Handle FCM Push Notifications
	if assigneeDrgraph != nil {
		eligibleUserIDs = append(eligibleUserIDs, assigneeDrgraph.Uuid)
	}
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewTaskNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = taskUUID
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = taskName
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.UserName
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	// Send notifications in batches of 500 tokens
	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendNewTaskNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out for task creation:
	//   1. Assignee gets a "you have been assigned" email.
	//   2. Mentioned project members get a mention email.
	if assigneeDrgraph != nil {
		notificationBusiness.DispatchTaskAssignment(
			userDgraph.Uuid,
			userDgraph.UserName,
			userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
			taskUUID,
			taskName,
			"",
			assigneeDrgraph.Uuid,
		)
	}
	// We do not currently have project name here — empty subtitle is fine.
}

func UpdateTaskDesByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskDesc string, mentionUsers []*dgraphStruct.DgraphUser, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {
	// Guard: skip no-op updates (normalize empty HTML paragraphs to empty string)
	normalizeDesc := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "<p></p>" || s == "<p><br></p>" {
			return ""
		}
		return s
	}
	oldDesc := ""
	if dgraphTaskInfo.Description != nil {
		oldDesc = normalizeDesc(*dgraphTaskInfo.Description)
	}
	newDesc := normalizeDesc(taskDesc)
	if oldDesc == newDesc {
		return nil
	}

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskDesByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	plainText := helpers.HTMLToPlainText(taskDesc)
	oldPlainText := ""
	if dgraphTaskInfo.Description != nil {
		oldPlainText = helpers.HTMLToPlainText(*dgraphTaskInfo.Description)

	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:         "uid(task)",
		Uuid:        taskUUID.String(),
		Description: &taskDesc,
		UpdatedAt:   &currentTime,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid:       "uid(me)",
			UpdatedAt: &currentTime,
			Mentions:  mentionUsers,
			TaskUuid:  taskUUID.String(),
		},
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_DESC,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: oldPlainText,
			NextState: plainText,
		}},
		DeletedAt: &zeroUnixTime,
	}
	_, err = domain.CreateOrUpdateDgraphTaskDescWithMentions(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskDesByTaskUUID failed to update task desc in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:            dgraphTask.Uuid,
		TaskUpdatedAt:   currentTime.Unix(),
		TaskDescription: &plainText,
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	assigneeUUID := ""
	if dgraphTaskInfo.Assignee != nil {
		assigneeUUID = dgraphTaskInfo.Assignee.Uuid
	}
	go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), assigneeUUID)

	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "description", map[string]interface{}{"description": taskDesc})

	return
}

func UpdateTaskNameByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskName string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskNameByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Name: taskName,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_NAME,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: dgraphTaskInfo.Name,
			NextState: taskName,
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskNameByTaskUUID failed to update task name in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          dgraphTask.Uuid,
		TaskUpdatedAt: currentTime.Unix(),
		TaskName:      taskName,
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	assigneeUUID := ""
	if dgraphTaskInfo.Assignee != nil {
		assigneeUUID = dgraphTaskInfo.Assignee.Uuid
	}
	go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), assigneeUUID)

	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "name", map[string]interface{}{"name": taskName})

	return
}

func UpdateTaskAssigneeByTaskUUID(ctx context.Context, taskUUID uuid.UUID, newAssigneeDgraphInfo *dgraphStruct.DgraphUser, dgraphOldUserUID string, dgraphTaskUID string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	activityUUID := uuid.New()

	// err = domain.UpdateTaskAssigneeByTaskUUID(taskUUID, taskAssigneeUUID, &currentTime)

	// if err != nil {
	// 	helpers.LogErrorWithContext(ctx,"business/UpdateTaskAssigneeByTaskUUID failed to update task assignee in postgres err: %+v", err)
	// 	return
	// }

	zeroUnixTime := time.Time{}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_ASSIGNEE,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime: &currentTime,
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	emptyString := ""

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:             dgraphTask.Uuid,
		TaskUpdatedAt:    currentTime.Unix(),
		TaskAssigneeUuid: &emptyString,
	}

	if dgraphTaskInfo.Assignee != nil {

		dgraphTask.Activity[0].PrevState = dgraphTaskInfo.Assignee.UserName

	}

	if newAssigneeDgraphInfo != nil {
		dgraphTask.Assignee = &dgraphStruct.DgraphUser{
			Uid: newAssigneeDgraphInfo.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		}

		dgraphTask.Activity[0].NextState = newAssigneeDgraphInfo.UserName

		taskOpenSearch.TaskAssigneeUuid = &newAssigneeDgraphInfo.Uuid
		taskOpenSearch.TaskAssigneeFullName = newAssigneeDgraphInfo.UserFullName
	}

	_, err = domain.UpdateDgraphTaskAssignee(ctx, dgraphTask, dgraphOldUserUID, dgraphTaskUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskAssigneeByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	if newAssigneeDgraphInfo != nil {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), newAssigneeDgraphInfo.Uuid)
	}
	if dgraphTaskInfo.Assignee != nil && dgraphTaskInfo.Assignee.Uuid != "" && (newAssigneeDgraphInfo == nil || dgraphTaskInfo.Assignee.Uuid != newAssigneeDgraphInfo.Uuid) {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), dgraphTaskInfo.Assignee.Uuid)
	}

	assigneeUUID := ""
	if newAssigneeDgraphInfo != nil {
		assigneeUUID = newAssigneeDgraphInfo.Uid
	}
	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "assignee", map[string]interface{}{"assignee_uuid": assigneeUUID})

	// Notify listeners that the task was assigned, so an AI teammate assigned
	// to work it can pick it up (durable agent-task queue). Only on a real
	// change to a new assignee. The assignee's USER uuid (not the dgraph uid)
	// is sent so the agent-principal resolver can match it. Loop-safe: an
	// agent's own writes are workflow-tagged and skipped by the event bus.
	if newAssigneeDgraphInfo != nil && newAssigneeDgraphInfo.Uuid != "" &&
		(dgraphTaskInfo.Assignee == nil || dgraphTaskInfo.Assignee.Uuid != newAssigneeDgraphInfo.Uuid) {
		go dispatchTaskAssignedEvent(context.WithoutCancel(ctx), taskUUID, dgraphTaskInfo, newAssigneeDgraphInfo.Uuid, userInfo)
	}

	// Email the new assignee, but only if it's an actual change (not a
	// re-assign to the same person, and not the actor assigning to themselves).
	if newAssigneeDgraphInfo != nil &&
		newAssigneeDgraphInfo.Uuid != "" &&
		newAssigneeDgraphInfo.Uuid != userInfo.Uuid &&
		(dgraphTaskInfo.Assignee == nil || dgraphTaskInfo.Assignee.Uuid != newAssigneeDgraphInfo.Uuid) {
		taskName := ""
		if dgraphTaskInfo.Name != "" {
			taskName = dgraphTaskInfo.Name
		}
		notificationBusiness.DispatchTaskAssignment(
			userInfo.Uuid,
			userInfo.UserName,
			userBusiness.GetSignedProfileURL(ctx, userInfo.ProfileKey),
			taskUUID.String(),
			taskName,
			"",
			newAssigneeDgraphInfo.Uuid,
		)
	}

	return
}

// dispatchTaskAssignedEvent emits the "task.assigned" event used to hand a task
// off to an AI teammate when the assignee is an agent principal. Best-effort;
// callers run it in a goroutine. The assignee's USER uuid is sent so the
// agent-principal resolver can match it.
func dispatchTaskAssignedEvent(ctx context.Context, taskUUID uuid.UUID, dgraphTaskInfo *dgraphStruct.DgraphTask, assigneeUUID string, actor *dgraphStruct.DgraphUser) {
	payload := map[string]interface{}{
		"task_id":     taskUUID.String(),
		"assignee_id": assigneeUUID,
	}
	if dgraphTaskInfo != nil {
		payload["name"] = dgraphTaskInfo.Name
		if dgraphTaskInfo.Description != nil {
			payload["description"] = *dgraphTaskInfo.Description
		}
		if dgraphTaskInfo.Project != nil {
			payload["project_id"] = dgraphTaskInfo.Project.Uuid
		}
	}
	if actor != nil {
		payload["assigned_by_name"] = actor.UserName
		// Who handed the work over: the person told when the agent needs them.
		payload["assigned_by"] = actor.Uuid
	}
	webhookBusiness.DispatchEvent(ctx, "task.assigned", payload)
}

func UpdateTaskDueDateByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskDueDate *time.Time, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskDueDateByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:     "uid(task)",
		Uuid:    taskUUID.String(),
		DueDate: taskDueDate,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_END_DATE,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: strconv.FormatInt(dgraphTaskInfo.DueDate.Unix(), 10),
			NextState: strconv.FormatInt(taskDueDate.Unix(), 10),
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskDueDateByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}

	if dgraphTaskInfo.Assignee != nil && dgraphTaskInfo.Assignee.Uuid != "" {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), dgraphTaskInfo.Assignee.Uuid)
	}

	return
}

func UpdateTaskStartDateByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskStartDate *time.Time, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskStartDateByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:       "uid(task)",
		Uuid:      taskUUID.String(),
		StartDate: taskStartDate,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_START_DATE,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: strconv.FormatInt(dgraphTaskInfo.StartDate.Unix(), 10),
			NextState: strconv.FormatInt(taskStartDate.Unix(), 10),
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskAssigneeByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}

	if dgraphTaskInfo.Assignee != nil && dgraphTaskInfo.Assignee.Uuid != "" {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), dgraphTaskInfo.Assignee.Uuid)
	}

	return
}

// UpdateTaskStatusByTaskUUID moves a task to a status: a built-in key or label,
// or one of its project's custom statuses by id or name (see
// business/TaskStatus). Every change of a task's status goes through here, so
// the activity log, search, GitHub sync and webhooks see all of them. Asking
// for the status the task is already in does nothing.
func UpdateTaskStatusByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskStatus string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {
	projectID := ""
	if dgraphTaskInfo.Project != nil {
		projectID = dgraphTaskInfo.Project.Uuid
	}
	next, err := taskStatusBusiness.Resolve(ctx, projectID, taskStatus)
	if err != nil {
		return err
	}
	prev := taskStatusBusiness.Of(dgraphTaskInfo)
	if prev.Category == next.Category && prev.CustomID == next.CustomID {
		return nil
	}

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskStatusByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	// The activity names statuses as they read: a built-in key the app turns
	// into its label, or a custom status's name as it was at the time.
	activityState := func(r taskStatusBusiness.Resolved) string {
		if r.CustomName != "" {
			return r.CustomName
		}
		return r.Category
	}
	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:    "uid(task)",
		Uuid:   taskUUID.String(),
		Status: next.Category,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_STATUS,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: activityState(prev),
			NextState: activityState(next),
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}
	var clear []string
	if next.CustomID != "" {
		dgraphTask.CustomStatus, dgraphTask.CustomStatusName = &next.CustomID, &next.CustomName
	} else if prev.CustomID != "" {
		clear = []string{"task_custom_status", "task_custom_status_name"}
	}

	err = domain.UpdateDgraphTaskClearing(ctx, dgraphTask, clear)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskStatusByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}
	dgraphTaskInfo.Status = next.Category
	dgraphTaskInfo.CustomStatus, dgraphTaskInfo.CustomStatusName = dgraphTask.CustomStatus, dgraphTask.CustomStatusName

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          taskUUID.String(),
		TaskStatus:    next.Category,
		TaskUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	// GitHub knows open and closed, which the category decides.
	if prev.Category != next.Category {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "status", map[string]interface{}{"status": next.Category})
	}

	// Settings, Notifications offers email for this; nothing sent it.
	var people []string
	projectName := ""
	if dgraphTaskInfo.Assignee != nil {
		people = append(people, dgraphTaskInfo.Assignee.Uuid)
	}
	if dgraphTaskInfo.CreatedBy != nil {
		people = append(people, dgraphTaskInfo.CreatedBy.Uuid)
	}
	if dgraphTaskInfo.Project != nil {
		projectName = dgraphTaskInfo.Project.Name
	}
	notificationBusiness.DispatchTaskStatusChange(userInfo.Uuid, userInfo.UserName,
		userBusiness.GetSignedProfileURL(ctx, userInfo.ProfileKey),
		taskUUID.String(), dgraphTaskInfo.Name, projectName, prev.Display(), next.Display(),
		activityUUID.String(), people)

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.status_changed", map[string]interface{}{
		"task_id":         taskUUID.String(),
		"old_status":      prev.Category,
		"new_status":      next.Category,
		"old_status_name": prev.Display(),
		"new_status_name": next.Display(),
		"old_custom_id":   prev.CustomID,
		"new_custom_id":   next.CustomID,
		"project_id":      projectID,
		"updated_by":      userInfo.Uid,
		// What a workflow's message says: "{by} moved {task} to {status}".
		"task_name":       dgraphTaskInfo.Name,
		"project_name":    projectName,
		"updated_by_name": userInfo.UserName,
		"updated_by_uuid": userInfo.Uuid,
	})

	// A repeating task makes its next occurrence when it is done.
	if next.Category == dgraphStruct.TASK_STATUS_DONE && prev.Category != dgraphStruct.TASK_STATUS_DONE {
		repeatIfRecurring(ctx, taskUUID, userInfo)
	}

	return
}

func UpdateTaskPriorityByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskPriority string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskPriorityByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:      "uid(task)",
		Uuid:     taskUUID.String(),
		Priority: taskPriority,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_PRIORITY,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			PrevState: dgraphTaskInfo.Priority,
			NextState: taskPriority,
		}},
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskPriorityByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          taskUUID.String(),
		TaskPriority:  taskPriority,
		TaskUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	return
}

func UpdateTaskLabelByTaskUUID(ctx context.Context, taskUUID uuid.UUID, taskLabel string, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {

	currentTime := time.Now()
	activityUUID := uuid.New()

	err = domain.UpdateTaskByTaskUUID(ctx, taskUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskLabelByTaskUUID failed to update task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:   "uid(task)",
		Uuid:  taskUUID.String(),
		Label: &taskLabel,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_LABEL,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime:   &currentTime,
			NextState: taskLabel,
		}},
		UpdatedAt: &currentTime,
	}

	if dgraphTaskInfo.Label != nil {
		dgraphTask.Activity[0].PrevState = *dgraphTaskInfo.Label
	}

	_, err = domain.CreateOrUpdateDgraphTaskLabel(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateTaskLabelByTaskUUID failed to update task in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          taskUUID.String(),
		TaskUpdatedAt: currentTime.Unix(),
		TaskLabel:     &taskLabel,
	}

	go domain.UpdateTaskInOpenSearch(taskOpenSearch)

	oldLabel := ""
	if dgraphTaskInfo.Label != nil {
		oldLabel = *dgraphTaskInfo.Label
	}
	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "label", map[string]interface{}{
		"label":     taskLabel,
		"old_label": oldLabel,
	})

	return
}

func ArchiveTaskByTaskUUID(ctx context.Context, taskUUID uuid.UUID, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {
	currentTime := time.Now()
	activityUUID := uuid.New()

	err = domain.UpdateTaskDeletedTimeByUUID(ctx, taskUUID, &currentTime, &currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveTaskByTaskUUID failed to update delete time for task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_DELETE_TASK,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime: &currentTime,
		}},
		DeletedAt: &currentTime,
	}

	if dgraphTaskInfo.ParentTask != nil {
		patentActivityUUID := uuid.New()

		dgraphTask.ParentTask = &dgraphStruct.DgraphTask{
			Uid: dgraphTaskInfo.ParentTask.Uid,
		}

		dgraphTask.ParentTask.Activity = []*dgraphStruct.DgraphTaskActivity{{
			Uuid: patentActivityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_DELETE_SUB_TASK,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime: &currentTime,
		}}
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveTaskByTaskUUID failed to update delete time for task in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          taskUUID.String(),
		TaskDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
		TaskUpdatedAt: currentTime.Unix(),
	}
	go domain.UpdateTaskWithAttachmentsInOpenSearch(taskOpenSearch, dgraphTaskInfo.Attachments)
	// Cascade to comments, attachments, and AI embeddings in one call
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_task_id", "attachment_task_id", "content_uuid", "task_uuid"},
		taskUUID.String(),
		currentTime.Unix(),
		[]string{"comments", "attachments", "ai_embeddings"},
		"cascade",
	)

	if dgraphTaskInfo.Assignee != nil && dgraphTaskInfo.Assignee.Uuid != "" {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), dgraphTaskInfo.Assignee.Uuid)
	}

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.deleted", map[string]interface{}{
		"task_id":    taskUUID.String(),
		"project_id": dgraphTaskInfo.Project.Uuid,
		"name":       dgraphTaskInfo.Name,
		"status":     dgraphTaskInfo.Status,
		"deleted_by": userInfo.Uid,
	})

	if dgraphTaskInfo.Status != "canceled" {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "status", map[string]interface{}{"status": "canceled"})
	}

	return
}

func UnArchiveTaskByTaskUUID(ctx context.Context, taskUUID uuid.UUID, dgraphTaskInfo *dgraphStruct.DgraphTask, userInfo *dgraphStruct.DgraphUser) (err error) {
	currentTime := time.Now()
	zeroUnixTime := time.Time{}
	activityUUID := uuid.New()

	err = domain.UpdateTaskDeletedTimeToNullByUUID(ctx, taskUUID, &currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveTaskByTaskUUID failed to update delete time for task in postgres err: %+v", err)
		return
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid: activityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_UNDELETE_TASK,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime: &currentTime,
		}},
		DeletedAt: &zeroUnixTime,
	}

	if dgraphTaskInfo.ParentTask != nil {
		patentActivityUUID := uuid.New()

		dgraphTask.ParentTask = &dgraphStruct.DgraphTask{
			Uid: dgraphTaskInfo.ParentTask.Uid,
		}

		dgraphTask.ParentTask.Activity = []*dgraphStruct.DgraphTaskActivity{{
			Uuid: patentActivityUUID.String(),
			Type: dgraphStruct.ACTIVITY_TYPE_UNDELETE_SUB_TASK,
			CreatedBy: &dgraphStruct.DgraphUser{
				Uid: userInfo.Uid,
			},
			LogTime: &currentTime,
		}}
	}

	_, err = domain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnArchiveTaskByTaskUUID failed to update delete time for task in dgraph err: %+v", err)
		return
	}

	taskOpenSearch := &openSearchStruct.OpenSearchTask{
		Uuid:          taskUUID.String(),
		TaskDeletedAt: nil,
		TaskUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateTaskWithAttachmentsInOpenSearch(taskOpenSearch, dgraphTaskInfo.Attachments)
	// Cascade unarchive to comments, attachments, and AI embeddings in one call
	go globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(
		[]string{"comment_task_id", "attachment_task_id", "content_uuid", "task_uuid"},
		taskUUID.String(),
		[]string{"comments", "attachments", "ai_embeddings"},
		"cascade",
	)

	if dgraphTaskInfo.Assignee != nil && dgraphTaskInfo.Assignee.Uuid != "" {
		go integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID.String(), dgraphTaskInfo.Assignee.Uuid)
	}

	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "status", map[string]interface{}{"status": dgraphTaskInfo.Status})

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "task.restored", map[string]interface{}{
		"task_id":     taskUUID.String(),
		"project_id":  dgraphTaskInfo.Project.Uuid,
		"name":        dgraphTaskInfo.Name,
		"restored_by": userInfo.Uid,
	})

	return
}

// ErrTaskNotVisible is returned when a task exists and the viewer may not see it.
//
// Shaped as "not found" rather than "forbidden" ON PURPOSE. This endpoint is
// addressed by task UUID, so a distinct "forbidden" would confirm that a given
// UUID names a real task, and a caller could walk the space learning which ids
// exist. A viewer with no claim to the task learns nothing either way.
var ErrTaskNotVisible = errors.New("task not found")

// CanViewTask reports whether this viewer may read this task.
//
// FOUR WAYS IN, because the product genuinely has four. Project membership is
// the common one, project admin is not implied by it (an admin need not sit in
// project_members), and the two that matter most are assignee and creator: a
// task can be assigned to somebody outside its project, and locking those people
// out of their own work is the failure mode that made the entity-link refusal so
// confusing to debug.
//
// A task with no project resolves to creator or assignee only, which is correct
// rather than a hole: an unparented task belongs to whoever made it.
func CanViewTask(task *dgraphStruct.DgraphTask, userDgraphUID string) bool {
	if task == nil || userDgraphUID == "" {
		return false
	}
	if p := task.Project; p != nil && (p.IsProjectMember > 0 || p.IsProjectAdmin > 0) {
		return true
	}
	if a := task.Assignee; a != nil && a.Uid == userDgraphUID {
		return true
	}
	if c := task.CreatedBy; c != nil && c.Uid == userDgraphUID {
		return true
	}
	return false
}

// GetDgraphTaskInfo reads a task AND enforces who may see it.
//
// THE CHECK LIVES HERE, not in the handler, because the handler is not the only
// caller. Eight paths read tasks through this function: the HTTP endpoint, the
// MCP reach tool, agent delegation, the agent work entity, and four pieces of
// background work. The HTTP endpoint had its membership check commented out and
// the others never had one, so an agent or an MCP client could read any task in
// the workspace by uuid. Fixing only the handler would have left those open.
//
// Deliberately NOT pushed into the Dgraph query, which is the textbook answer.
// Doing that here means var blocks and reverse edges replicated across roughly
// twenty task queries in a language where a mistake either leaks everything or
// locks everyone out, and the four signals the decision needs are already in
// this response. The service layer is where the application has the full context
// to decide, and one place to decide it.
func GetDgraphTaskInfo(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	dgraphTask, err = domain.GetDgraphTaskInfoByUUID(ctx, teamUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTaskInfo failed to get task info from dgraph err: %+v", err)
		return
	}
	if dgraphTask == nil {
		return nil, nil
	}

	// Background work has no user whose membership could be checked, and says so
	// explicitly rather than passing a placeholder uid that looks like a person.
	if helpers.IsSystemRead(ctx) {
		return dgraphTask, nil
	}

	if !CanViewTask(dgraphTask, userDgraphUID) {
		return nil, ErrTaskNotVisible
	}
	return dgraphTask, nil
}

func GetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID) (status string, errMsg *string, attempts int, err error) {
	status, errMsg, attempts, err = domain.GetGitHubSyncStatus(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetGitHubSyncStatus Failed err: %+v", err)
		return "", nil, 0, errors.New("failed to get sync status")
	}
	return
}

func SetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID, status string, errMsg *string, attempts int) error {
	err := domain.SetGitHubSyncStatus(ctx, taskUUID, status, errMsg, attempts)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SetGitHubSyncStatus Failed err: %+v", err)
		return errors.New("failed to set sync status")
	}
	return nil
}

// MergeGitHubMetaIntoTask fetches GitHub metadata from PostgreSQL and overlays it onto the Dgraph task.
func MergeGitHubMetaIntoTask(ctx context.Context, taskUUID uuid.UUID, task *dgraphStruct.DgraphTask) error {
	meta, err := domain.GetGitHubMetaForTask(ctx, taskUUID)
	if err != nil || meta == nil {
		return err
	}

	task.GitHubIssueNumber = meta.IssueNumber
	task.GitHubIssueURL = meta.IssueURL
	task.GitHubPRNumber = meta.PRNumber
	task.GitHubPRURL = meta.PRURL
	task.GitHubBranch = meta.Branch
	task.GitHubPRState = meta.PRState
	task.GitHubPRCheckStatus = meta.PRCheckStatus
	task.GitHubPRReviewState = meta.PRReviewState
	task.GitHubPRIsDraft = meta.PRIsDraft
	return nil
}

// MergeGitHubMetaIntoTasks batch-fetches GitHub metadata and overlays it onto multiple Dgraph tasks.
func MergeGitHubMetaIntoTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	if len(tasks) == 0 {
		return nil
	}
	var taskUUIDs []uuid.UUID
	for _, t := range tasks {
		uid, err := uuid.Parse(t.Uuid)
		if err != nil {
			continue
		}
		taskUUIDs = append(taskUUIDs, uid)
	}
	if len(taskUUIDs) == 0 {
		return nil
	}
	metaMap, err := domain.GetGitHubMetaForTasks(ctx, taskUUIDs)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		uid, _ := uuid.Parse(t.Uuid)
		meta, ok := metaMap[uid]
		if !ok {
			continue
		}
		t.GitHubIssueNumber = meta.IssueNumber
		t.GitHubIssueURL = meta.IssueURL
		t.GitHubPRNumber = meta.PRNumber
		t.GitHubPRURL = meta.PRURL
		t.GitHubBranch = meta.Branch
		t.GitHubPRState = meta.PRState
		t.GitHubPRCheckStatus = meta.PRCheckStatus
		t.GitHubPRReviewState = meta.PRReviewState
		t.GitHubPRIsDraft = meta.PRIsDraft
	}
	return nil
}

func DeleteReactionOnCommentTask(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionDgraphUUID string) (err error) {
	err = businessComment.DeleteCommentReaction(ctx, commentDgraph.Uid, reactionDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteReactionOnCommentTask Failed to delete reaction on comment in post err: %+v",
			err)

		return
	}

	mqttTaskCommentReaction := mqttStruct.MqttTaskCommentReaction{
		Type:         mqttStruct.TYPE_DELETE,
		CommentUuid:  commentDgraph.Uuid,
		AddedByUuid:  commentDgraph.CommentBy.Uuid,
		ReactionUuid: reactionDgraphUUID,
		TaskUuid:     commentDgraph.Task.Uuid,
	}

	go mqttBusiness.PublishTaskCommentReaction(&mqttTaskCommentReaction, commentDgraph.Task.Project.Uuid)

	return
}

func CreateOrUpdateTaskCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInTask, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	currentTime := time.Now()

	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  reactionInfo.Uuid,
		DType: []string{"Comment"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiUuid,
				AddedAt:   &currentTime,
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: dgraphCommentRaw.CommentBy.Uid,
				},
				AddedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraph.Uid,
				},
			},
		},
	}

	reactionUUID, err = businessComment.CreateOrUpdateDgraphCommentReaction(ctx, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateTaskCommentReaction Failed to update comment reaction in post err: %+v",
			err)
		return
	}

	mqttPostCommentReaction := mqttStruct.MqttTaskCommentReaction{
		Type:            mqttStruct.TYPE_CREATE,
		CommentUuid:     reactionInfo.Uuid,
		EmojiReactionId: reactionInfo.EmojiUuid,
		AddedByUuid:     userDgraph.Uuid,
		AddedByUserName: userDgraph.UserName,
		ReactionUuid:    reactionUUID,
		TaskUuid:        dgraphCommentRaw.Task.Uuid,
	}

	if len(mqttPostCommentReaction.ReactionUuid) == 0 {
		mqttPostCommentReaction.Type = mqttStruct.TYPE_UPDATE
		mqttPostCommentReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	go mqttBusiness.PublishTaskCommentReaction(&mqttPostCommentReaction, dgraphCommentRaw.Task.Project.Uuid)

	return
}

func GetDgraphBasicTaskInfo(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	dgraphTask, err = domain.GetDgraphBasicTaskInfoByUUID(ctx, teamUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphBasicTaskInfo failed to get task info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphBasicTaskInfoWithAttachmentsByUUID(ctx context.Context, teamUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	dgraphTask, err = domain.GetDgraphBasicTaskInfoWithAttachmentsByUUID(ctx, teamUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphBasicTaskInfoWithAttachmentsByUUID failed to get task info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphTaskActivityList(ctx context.Context, taskUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	dgraphTask, err = domain.GetDgraphTaskActivityList(ctx, taskUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTaskActivityList failed to get task info from dgraph err: %+v", err)
		return
	}

	return
}

func GetDgraphTaskActivityInfo(ctx context.Context, teamUUID string, activityUUID string, userDgraphUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {
	dgraphTask, err = domain.GetDgraphTaskActivityInfo(ctx, teamUUID, activityUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphTaskActivityInfo failed to get task info from dgraph err: %+v", err)
		return
	}

	return
}
