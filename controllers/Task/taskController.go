package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	syncBusiness "github.com/akashc777/OneCamp/business"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	business "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"

	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func DeleteTaskComment(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var deleteTaskCommentInfoInput adapter.CreateOrUpdateTaskCommentInput

	err := json.NewDecoder(r.Body).Decode(&deleteTaskCommentInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskComment Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	isComentOwner := false

	dgraphCommentInfo, err := commentBusiness.GetDgraphCommentInfoByUUID(ctx, deleteTaskCommentInfoInput.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskCommentBody Failed to get dgraph comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph comment info",
			"err": err,
		})
		return

	}

	commentUUID, err := uuid.Parse(deleteTaskCommentInfoInput.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteTaskCommentBody Failed to parse string to uuid err: %+v",
			err)
		return
	}

	if dgraphCommentInfo.CommentBy.Uuid == userInfo.UserDgraphInfo.Uuid {
		isComentOwner = true
	}

	if dgraphCommentInfo.Task.Project.IsProjectAdmin == 0 && !isComentOwner {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.ArchiveCommentByCommentUUID(ctx, commentUUID, dgraphCommentInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskCommentBody Failed to parse commentUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse commentUUID",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted comment!"})

}

func UpdateTaskCommentBody(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskCommentInfoInput adapter.CreateOrUpdateTaskCommentInput

	err := json.NewDecoder(r.Body).Decode(&createTaskCommentInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskCommentBody Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskCommentInfoInput.CommentBody) == 0 || len(createTaskCommentInfoInput.Uuid) == 0 || len(createTaskCommentInfoInput.TaskUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	commentUUID, err := uuid.Parse(createTaskCommentInfoInput.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskCommentBody Failed to parse commentUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse commentUUID",
			"err": err,
		})
		return

	}

	dgraphCommentInfo, err := commentBusiness.GetDgraphCommentInfoByUUID(ctx, createTaskCommentInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskCommentBody Failed to get comment info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment info",
			"err": err,
		})
		return
	}

	isComentOwner := false

	if dgraphCommentInfo.CommentBy.Uuid == userInfo.UserDgraphInfo.Uuid {
		isComentOwner = true
	}

	if !isComentOwner {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	mentions, err := helpers.GetMentions(createTaskCommentInfoInput.CommentBody)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskCommentBody Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	err = business.UpdateTaskCommentBody(ctx, commentUUID, &createTaskCommentInfoInput, dgraphCommentInfo, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to create comment err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated comment!"})

}

func CreateTaskComment(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskCommentInfoInput adapter.CreateOrUpdateTaskCommentInput

	err := json.NewDecoder(r.Body).Decode(&createTaskCommentInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskCommentInfoInput.CommentBody) == 0 || len(createTaskCommentInfoInput.TaskUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskCommentInfoInput.TaskUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskCommentInfoInput.TaskUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to get dgraph task info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph task info err",
			"err": err,
		})
		return
	}

	if dgraphTaskInfo.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	mentions, err := helpers.GetMentions(createTaskCommentInfoInput.CommentBody)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	createCommentInfo, err := business.CreateTaskComment(ctx, taskUUID, dgraphTaskInfo, &userInfo, &createTaskCommentInfoInput, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTaskComment Failed to create comment err: %+v dgraphdgdg %+v",
			err, dgraphTaskInfo)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created comment!", "data": createCommentInfo})

}

func CreateTask(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	projectUUID, err := uuid.Parse(createTaskInfoInput.ProjectUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateProject Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, createTaskInfoInput.ProjectUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTask Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphProjectInfo.IsProjectAdmin == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	if len(createTaskInfoInput.Priority) == 0 {
		createTaskInfoInput.Priority = dgraphStruct.TASK_PRIORITY_MEDIUM
	}

	if createTaskInfoInput.Priority != dgraphStruct.TASK_PRIORITY_LOW && createTaskInfoInput.Priority != dgraphStruct.TASK_PRIORITY_MEDIUM && createTaskInfoInput.Priority != dgraphStruct.TASK_PRIORITY_HIGH {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid priority",
			"err": err,
		})
		return
	}

	createTaskInfoInput.Status = dgraphStruct.TASK_STATUS_TODO

	var assigneeDgraphInfo *dgraphStruct.DgraphUser

	if userInfo.UserDgraphInfo.Uuid == createTaskInfoInput.AssigneeUuid {
		assigneeDgraphInfo = &userInfo.UserDgraphInfo
	}

	if len(createTaskInfoInput.AssigneeUuid) > 0 && userInfo.UserDgraphInfo.Uuid != createTaskInfoInput.AssigneeUuid {
		dgraphAsigneeUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, createTaskInfoInput.AssigneeUuid)

		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/CreateTask Failed to get asignee dgraph info err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get asignee dgraph info",
				"err": err,
			})
			return

		}
		assigneeDgraphInfo = dgraphAsigneeUser
	}

	var mentionUsers []*dgraphStruct.DgraphUser

	if len(createTaskInfoInput.TaskDescription) > 0 {
		mentionsUUIDs, err := helpers.GetMentions(createTaskInfoInput.TaskDescription)

		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/CreateTask Failed to mention users uuid from task desc err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to mention users uuid from task desc",
				"err": err,
			})
			return

		}

		mentionUsers, err = userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentionsUUIDs)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/CreateTask Failed to get users mentions info err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get users mentions info",
				"err": err,
			})
			return

		}

	}

	taskUUID, err := business.CreateTask(ctx, projectUUID, &userInfo, dgraphProjectInfo, assigneeDgraphInfo, createTaskInfoInput, mentionUsers)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTask Failed to create task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create task",
			"err": err,
		})
		return

	}

	// The id lets the caller link to what it just made, e.g. a "Made this a
	// task" reply under the message the task came from.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created task!", "data": helpers.Envolope{"task_uuid": taskUUID.String()}})

}

func AddAttachmentsToTask(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 || len(createTaskInfoInput.Attachments) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToTask Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(createTaskInfoInput.ProjectUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToTask Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	var objKeys []string

	for _, attachment := range createTaskInfoInput.Attachments {
		objKeys = append(objKeys, attachment.ObjectKey)
	}

	err = business.AddAttachmentToTask(ctx, taskUUID, dgraphTaskInfo, &createTaskInfoInput, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToTask Failed to add attachment to task err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add attachment to task",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added attachment to task!"})

}

func RemoveAttachmentFromTask(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var removeTaskAttachmentInput adapter.RemoveTaskAttachmentInput

	err := json.NewDecoder(r.Body).Decode(&removeTaskAttachmentInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	attachmentInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, removeTaskAttachmentInput.AttachmentObjKey, postgressStruct.ATTACHMENT_SRC_PROJECT)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to get attachment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachment info",
			"err": err,
		})
		return

	}

	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfoWithTaskUUID(ctx, attachmentInfo.SrcValue, userInfo.UserDgraphInfo.Uid, removeTaskAttachmentInput.TaskUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphProjectInfo.IsProjectAdmin == 0 || len(dgraphProjectInfo.Tasks) == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.RemoveAttachmentFromTask(ctx, attachmentInfo.Uuid, dgraphProjectInfo.Tasks[0].Uid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to delete attachmnet in task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete attachmnet in task ",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted task's attachment"})

}

func GetTaskInfo(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	taskUUIDString := chi.URLParam(r, "task_uuid")

	taskUUID, err := uuid.Parse(taskUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphTaskInfo(ctx, taskUUIDString, userInfo.UserDgraphInfo.Uid)

	// A task the viewer may not see answers exactly as a task that does not
	// exist. Anything else would let a caller learn which uuids are real by
	// comparing the two replies, which is the only thing an endpoint addressed
	// by uuid can leak.
	if errors.Is(err, business.ErrTaskNotVisible) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "Task not found",
		})
		return
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetTaskInfo Failed to get dgraph task info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph task info err",
			"err": err,
		})
		return
	}

	// Merge ephemeral GitHub metadata from PostgreSQL into the Dgraph response
	if dgraphTaskInfo != nil {
		_ = business.MergeGitHubMetaIntoTask(ctx, taskUUID, dgraphTaskInfo)
	}

	// if dgraphTaskInfo == nil || dgraphTaskInfo.Project.IsProjectMember == 0 {
	// 	helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
	// 		"msg": "Not Authorised",
	// 	})
	// 	return
	// }

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got task info", "data": dgraphTaskInfo})

}

func UpdateTaskName(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskName Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.TaskName) == 0 || len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_name, task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskName Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskName Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.UpdateTaskNameByTaskUUID(ctx, taskUUID, createTaskInfoInput.TaskName, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskName Failed to update task name in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task name in dgraph",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task name!"})

}

func UpdateTaskDesc(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDesc Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDesc Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDesc Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	var mentionUsers []*dgraphStruct.DgraphUser

	if len(createTaskInfoInput.TaskDescription) > 0 {
		mentionsUUIDs, err := helpers.GetMentions(createTaskInfoInput.TaskDescription)

		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskDesc Failed to mention users uuid from task desc err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to mention users uuid from task desc",
				"err": err,
			})
			return

		}

		mentionUsers, err = userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentionsUUIDs)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskDesc Failed to get users mentions info err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get users mentions info",
				"err": err,
			})
			return

		}

	}

	err = business.UpdateTaskDesByTaskUUID(ctx, taskUUID, createTaskInfoInput.TaskDescription, mentionUsers, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDesc Failed to update task desc in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task desc in dgraph",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task description!"})

}

func UpdateTaskAssignee(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskAssignee Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskAssignee Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	var newAssigneeDgraphInfo *dgraphStruct.DgraphUser

	if len(createTaskInfoInput.AssigneeUuid) > 0 {
		_, err = uuid.Parse(createTaskInfoInput.AssigneeUuid)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskAssignee Failed to parse userUUID string to uuid err: %+v",
				err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse userUUID string to uuid",
				"err": err,
			})
			return
		}

		dgraphAssigneeUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, createTaskInfoInput.AssigneeUuid)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskAssignee Failed to get task info from dgraph err: %+v",
				err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get task info from dgraph",
				"err": err,
			})
			return
		}

		newAssigneeDgraphInfo = dgraphAssigneeUser
	}

	dgraphTask, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskAssignee Failed to get task info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task info from dgraph",
			"err": err,
		})
		return
	}

	if dgraphTask.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	oldAssigneeDgraphUID := ""

	if dgraphTask.Assignee != nil {
		oldAssigneeDgraphUID = dgraphTask.Assignee.Uid
	}

	err = business.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, newAssigneeDgraphInfo, oldAssigneeDgraphUID, dgraphTask.Uid, dgraphTask, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskAssignee Failed to update task desc in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task desc in dgraph",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task Assignee!"})

}

func UpdateTaskDueDate(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDueDate Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDueDate Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDueDate Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	taskDueTime := time.Time{}

	if len(createTaskInfoInput.DueDate) > 0 {
		taskDueTime, err = time.Parse(time.RFC3339, createTaskInfoInput.DueDate)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskDueDate Failed parse task due time err: %+v", //'/
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg":    "Failed parse task due time",
				"err":    err,
				"status": "failed",
			})
			return

		}
	}

	err = business.UpdateTaskDueDateByTaskUUID(ctx, taskUUID, &taskDueTime, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskDueDate Failed to update task due time in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task due time",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task due time!"})

}

func UpdateTaskStartDate(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStartDate Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStartDate Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStartDate Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	taskStartTime := time.Time{}

	if len(createTaskInfoInput.StartDate) > 0 {
		taskStartTime, err = time.Parse(time.RFC3339, createTaskInfoInput.StartDate)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/UpdateTaskStartDate Failed parse task start time err: %+v", //'/
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg":    "Failed parse task start time",
				"err":    err,
				"status": "failed",
			})
			return

		}
	}

	err = business.UpdateTaskStartDateByTaskUUID(ctx, taskUUID, &taskStartTime, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStartDate Failed to update task start time in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task start time",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task start time!"})

}

func CreateOrUpdateReactionOnCommentTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentTaskReactionInfo adapter.InputUpdateReactionForCommentInTask

	err := json.NewDecoder(r.Body).Decode(&commentTaskReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphTaskCommentInfoByUUID(ctx, commentTaskReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphComment == nil || !dgraphComment.Task.Project.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentTask Failed to comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add reaction",
			"err": err,
		})
		return
	}

	if dgraphComment.Task.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized ",
		})
		return
	}

	commentTaskReactionInfo.ReactionDgraphUid, err = business.CreateOrUpdateTaskCommentReaction(ctx, &commentTaskReactionInfo, dgraphComment, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentTask Failed to create/update reaction on post comment req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated reaction successfully!", "data": commentTaskReactionInfo})
}

func DeleteTaskCommentReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var commentReactionInfo adapter.InputDeleteReactionForTaskComment

	err := json.NewDecoder(r.Body).Decode(&commentReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskCommentReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentInfo, err := commentBusiness.GetDgraphTaskCommentInfoByUUID(ctx, commentReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || !commentInfo.Task.Project.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskCommentReaction Failed to get dgraph comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get dgraph comment info",
			"err": err,
		})
		return
	}

	if commentInfo.Task.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	reactionDgraph, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, commentReactionInfo.ReactionDgraphUid)

	if reactionDgraph.AddedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	err = business.DeleteReactionOnCommentTask(ctx, commentInfo, commentReactionInfo.ReactionDgraphUid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteTaskCommentReaction Failed to delete task on the comment req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

}

func UpdateTaskStatus(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStatus Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Status) == 0 || len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_status, task_uuid and task_project_uuid are required",
		})

		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStatus Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStatus Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.UpdateTaskStatusByTaskUUID(ctx, taskUUID, createTaskInfoInput.Status, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "That is not a status in this project. Use one of: " + taskStatusBusiness.Describe(ctx, dgraphTaskInfo.Project.Uuid),
		})
		return
	}
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskStatus Failed to update task status in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task status",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task status!"})

}

func UpdateTaskPriority(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskPriority Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Priority) == 0 || len(createTaskInfoInput.Uuid) == 0 || len(createTaskInfoInput.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "task_priority, task_uuid and task_project_uuid are required",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskPriority Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskPriority Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	isValidPriority := false

	for _, taskPriority := range dgraphStruct.VALID_TASK_PRIORITIES {
		if taskPriority == createTaskInfoInput.Priority {
			isValidPriority = true
			break
		}
	}

	if !isValidPriority {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not authorised invalid priority",
		})
		return
	}

	err = business.UpdateTaskPriorityByTaskUUID(ctx, taskUUID, createTaskInfoInput.Priority, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskPriority Failed to update task priority in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task priority",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task priority!"})

}

func UpdateTaskLabel(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskLabel Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(createTaskInfoInput.Uuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	taskUUID, err := uuid.Parse(createTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskLabel Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, createTaskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskLabel Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.UpdateTaskLabelByTaskUUID(ctx, taskUUID, createTaskInfoInput.Label, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTaskLabel Failed to update task label in dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update task label",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task label!"})

}

func CreateSubTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createSubTaskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&createSubTaskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateSubTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfoWithTaskUUID(ctx, createSubTaskInfoInput.ProjectUuid, userInfo.UserDgraphInfo.Uid, createSubTaskInfoInput.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateSubTask Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphProjectInfo.IsProjectAdmin == 0 || len(dgraphProjectInfo.Tasks) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	var assigneeDgraphInfo *dgraphStruct.DgraphUser

	if userInfo.UserDgraphInfo.Uuid == createSubTaskInfoInput.AssigneeUuid {
		assigneeDgraphInfo = &userInfo.UserDgraphInfo
	}

	if len(createSubTaskInfoInput.AssigneeUuid) > 0 && userInfo.UserDgraphInfo.Uuid != createSubTaskInfoInput.AssigneeUuid {
		dgraphAsigneeUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, createSubTaskInfoInput.AssigneeUuid)

		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/CreateTask Failed to get asignee dgraph info err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get asignee dgraph info",
				"err": err,
			})
			return

		}
		assigneeDgraphInfo = dgraphAsigneeUser
	}

	_, err = uuid.Parse(createSubTaskInfoInput.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateSubTask Failed to parse taskUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse taskUUID string to uuid",
			"err": err,
		})
		return
	}

	projectUUID, err := uuid.Parse(createSubTaskInfoInput.ProjectUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateSubTask Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	subTaskDgraph, err := business.CreateSubTask(ctx, projectUUID, &userInfo, dgraphProjectInfo, assigneeDgraphInfo, createSubTaskInfoInput)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateSubTask Failed to create sub task err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create sub task",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created sub task!", "data": subTaskDgraph})

}

func GetTaskActivityList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	taskUUIDString := chi.URLParam(r, "task_uuid")

	_, err := uuid.Parse(taskUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse task string to uuid",
			"err": err,
		})
		return
	}

	dgraphTask, err := business.GetDgraphTaskActivityList(ctx, taskUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetTaskActivityList Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTask.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got task activity list!", "data": dgraphTask})

}

func GetTaskActivityInfo(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	taskActivityUUIDString := chi.URLParam(r, "activity_uuid")

	_, err := uuid.Parse(taskActivityUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse activity string to uuid",
			"err": err,
		})
		return
	}

	taskUUIDString := chi.URLParam(r, "task_uuid")

	_, err = uuid.Parse(taskUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse task string to uuid",
			"err": err,
		})
		return
	}

	dgraphTask, err := business.GetDgraphTaskActivityInfo(ctx, taskUUIDString, taskActivityUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetTaskActivityList Failed to get task dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get task dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTask.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got task activity info!", "data": dgraphTask})

}

func ArchiveTask(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var taskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&taskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	taskUUID, err := uuid.Parse(taskInfoInput.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTask Failed to parse string to uuid err: %+v",
			err)
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfoWithAttachmentsByUUID(ctx, taskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTask Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.ArchiveTaskByTaskUUID(ctx, taskUUID, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTask Failed to archive task err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to archive task",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Archived task!"})

}

func UnArchiveTask(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var taskInfoInput adapter.CreateOrUpdateTaskInput

	err := json.NewDecoder(r.Body).Decode(&taskInfoInput)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTask Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	taskUUID, err := uuid.Parse(taskInfoInput.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTask Failed to parse string to uuid err: %+v",
			err)
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfoWithAttachmentsByUUID(ctx, taskInfoInput.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTask Failed to get project dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	if dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	err = business.UnArchiveTaskByTaskUUID(ctx, taskUUID, dgraphTaskInfo, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTask Failed to unarchive task err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to archive task",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "UnArchived task!"})

}

// LinkTaskToGitHub links an existing task to a GitHub issue or PR.
func LinkTaskToGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid task UUID"})
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	owner, repo, number, err := githubBusiness.ExtractGitHubIssueInfo(req.URL)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid GitHub URL"})
		return
	}

	// Validate that the repo is linked to the task's project
	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.Project == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Task or project not found"})
		return
	}
	projectUUID, _ := uuid.Parse(dgraphTaskInfo.Project.Uuid)
	links, err := githubBusiness.GetLinkedRepos(ctx, projectUUID)
	if err != nil || len(links) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No GitHub repo linked to this project"})
		return
	}
	var repoLinked bool
	for _, link := range links {
		if strings.EqualFold(link.RepoOwner, owner) && strings.EqualFold(link.RepoName, repo) {
			repoLinked = true
			break
		}
	}
	if !repoLinked {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "This repository is not linked to the task's project"})
		return
	}

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	// Auto-refreshing client. Falls back to the raw token only if the
	// refresh path is unconfigured (legacy non-rotating OAuth App).
	client, err := githubBusiness.GitHubHTTPClient(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	// Verify the issue/PR exists on GitHub
	ghURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number)
	ghReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ghURL, nil)
	ghReq.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(ghReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub issue or PR not found"})
		return
	}
	defer resp.Body.Close()

	var ghIssue struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	json.NewDecoder(resp.Body).Decode(&ghIssue)

	// Prevent duplicate linking to another task
	isPR := strings.Contains(req.URL, "/pull/")
	var existingTaskUUID string
	if isPR {
		existingTaskUUID, _ = taskDomain.FindTaskUUIDByPRURL(ctx, ghIssue.HTMLURL)
	} else {
		existingTaskUUID, _ = taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, ghIssue.HTMLURL)
	}
	if existingTaskUUID != "" && existingTaskUUID != taskUUIDStr {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This issue or PR is already linked to another task"})
		return
	}

	if isPR {
		taskDomain.SetGitHubPRFieldsOnTask(ctx, taskUUID, number, ghIssue.HTMLURL, "")
	} else {
		taskDomain.SetGitHubIssueFieldsOnTask(ctx, taskUUID, number, ghIssue.HTMLURL)
	}

	// Sync to Dgraph
	if dgraphTaskInfo != nil {
		var dgTask *dgraphStruct.DgraphTask
		if isPR {
			dgTask = &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUIDStr, GitHubPRNumber: &number, GitHubPRURL: &ghIssue.HTMLURL}
		} else {
			dgTask = &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUIDStr, GitHubIssueNumber: &number, GitHubIssueURL: &ghIssue.HTMLURL}
		}
		taskDomain.CreateOrUpdateDgraphTask(ctx, dgTask)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Linked to GitHub successfully"})
}

// UnlinkTaskFromGitHub removes GitHub metadata from a task.
func UnlinkTaskFromGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid task UUID"})
		return
	}

	taskDomain.ClearGitHubIssueFieldsFromTask(ctx, taskUUID)

	// Also clear Dgraph fields
	dgTask := &dgraphStruct.DgraphTask{
		Uid:               "uid(task)",
		Uuid:              taskUUIDStr,
		GitHubIssueNumber: nil,
		GitHubIssueURL:    nil,
		GitHubPRNumber:    nil,
		GitHubPRURL:       nil,
		GitHubBranch:      nil,
	}
	taskDomain.CreateOrUpdateDgraphTask(ctx, dgTask)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Unlinked from GitHub"})
}

// CreateGitHubBranchForTask creates a branch on the linked repo and associates it with the task.
func CreateGitHubBranchForTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	_, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid task UUID"})
		return
	}

	var req struct {
		BranchName string `json:"branch_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BranchName == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "branch_name is required"})
		return
	}

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	client, err := githubBusiness.GitHubHTTPClient(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	// Find the linked repo for this task's project
	taskInfo, err := business.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userInfo.UserDgraphInfo.Uid)
	if err != nil || taskInfo == nil || taskInfo.Project == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Task or project not found"})
		return
	}
	projectUUID, _ := uuid.Parse(taskInfo.Project.Uuid)
	links, err := githubBusiness.GetLinkedRepos(ctx, projectUUID)
	if err != nil || len(links) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No GitHub repo linked to this project"})
		return
	}
	link := links[0]

	// Get default branch SHA to create new branch from
	repoURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/main", link.RepoOwner, link.RepoName)
	ghReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, repoURL, nil)
	ghReq.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(ghReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		// Fallback to master
		repoURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/master", link.RepoOwner, link.RepoName)
		ghReq, _ = http.NewRequestWithContext(ctx, http.MethodGet, repoURL, nil)
		ghReq.Header.Set("Accept", "application/vnd.github+json")
		resp, err = client.Do(ghReq)
		if err != nil || resp.StatusCode != http.StatusOK {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Could not find default branch"})
			return
		}
	}
	defer resp.Body.Close()

	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	json.NewDecoder(resp.Body).Decode(&ref)

	// Create the branch
	createRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs", link.RepoOwner, link.RepoName)
	payload, _ := json.Marshal(map[string]string{
		"ref": "refs/heads/" + req.BranchName,
		"sha": ref.Object.SHA,
	})
	createReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, createRefURL, strings.NewReader(string(payload)))
	createReq.Header.Set("Accept", "application/vnd.github+json")
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil || (createResp.StatusCode != http.StatusCreated && createResp.StatusCode != http.StatusOK) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to create branch on GitHub"})
		return
	}
	defer createResp.Body.Close()

	// Update task with branch
	taskUUID, _ := uuid.Parse(taskUUIDStr)
	taskDomain.SetGitHubBranchOnTaskByTaskID(ctx, req.BranchName, taskUUID)
	dgTask := &dgraphStruct.DgraphTask{
		Uid:          "uid(task)",
		Uuid:         taskUUIDStr,
		GitHubBranch: &req.BranchName,
	}
	taskDomain.CreateOrUpdateDgraphTask(ctx, dgTask)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Branch created successfully"})
}

// GetGitHubTaskActivity returns the GitHub activity timeline for a task.
func GetGitHubTaskActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid task UUID"})
		return
	}

	activities, err := githubTaskActivityDomain.GetGitHubTaskActivitiesByTaskID(ctx, taskUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"activities": activities})
}

// GetGitHubPRReviews returns PR reviews for a task.
func GetGitHubPRReviews(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid task UUID"})
		return
	}

	// Verify user has access to the task's project
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.Project == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Task not found"})
		return
	}
	if dgraphTaskInfo.Project.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"error": "Not authorized"})
		return
	}

	reviews, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(ctx, taskUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"reviews": reviews})
}

// GetGitHubSyncStatus returns the GitHub sync status for a task.
func GetGitHubSyncStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid task UUID",
		})
		return
	}

	status, errMsg, attempts, err := business.GetGitHubSyncStatus(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetGitHubSyncStatus Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to get sync status",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got sync status",
		"data": map[string]interface{}{
			"status":   status,
			"error":    errMsg,
			"attempts": attempts,
		},
	})
}

// SearchGitHubIssues searches for GitHub issues in the linked repo by title.
func SearchGitHubIssues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid task UUID"})
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Query parameter 'q' is required"})
		return
	}

	projectID, err := taskDomain.GetTaskProjectID(ctx, taskUUID)
	if err != nil || projectID == uuid.Nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Task has no project"})
		return
	}

	links, err := githubBusiness.GetLinkedRepos(ctx, projectID)
	if err != nil || len(links) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No linked GitHub repo for project"})
		return
	}
	link := links[0]

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	client, err := githubBusiness.GitHubHTTPClient(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	searchURL := fmt.Sprintf("https://api.github.com/search/issues?q=%s+repo:%s/%s+is:issue&sort=updated&order=desc&per_page=10",
		url.QueryEscape(query), link.RepoOwner, link.RepoName)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		helpers.WriteJSON(w, resp.StatusCode, helpers.Envolope{"error": string(body)})
		return
	}

	var result struct {
		Items []struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			HTMLURL   string `json:"html_url"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"issues": result.Items})
}

// SearchGitHubPRs searches for GitHub PRs in the linked repo by title.
func SearchGitHubPRs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid task UUID"})
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Query parameter 'q' is required"})
		return
	}

	projectID, err := taskDomain.GetTaskProjectID(ctx, taskUUID)
	if err != nil || projectID == uuid.Nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Task has no project"})
		return
	}

	links, err := githubBusiness.GetLinkedRepos(ctx, projectID)
	if err != nil || len(links) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No linked GitHub repo for project"})
		return
	}
	link := links[0]

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	client, err := githubBusiness.GitHubHTTPClient(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "GitHub not connected"})
		return
	}

	searchURL := fmt.Sprintf("https://api.github.com/search/issues?q=%s+repo:%s/%s+is:pr&sort=updated&order=desc&per_page=10",
		url.QueryEscape(query), link.RepoOwner, link.RepoName)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		helpers.WriteJSON(w, resp.StatusCode, helpers.Envolope{"error": string(body)})
		return
	}

	var result struct {
		Items []struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			HTMLURL   string `json:"html_url"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"prs": result.Items})
}

// BulkUnlinkTasksFromGitHub removes GitHub metadata from multiple tasks.
func BulkUnlinkTasksFromGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		TaskUUIDs []string `json:"task_uuids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	if len(input.TaskUUIDs) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No task UUIDs provided"})
		return
	}

	if len(input.TaskUUIDs) > 100 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Cannot process more than 100 tasks at once"})
		return
	}

	var successCount int
	var errors []string

	for _, taskUUIDStr := range input.TaskUUIDs {
		taskUUID, err := uuid.Parse(taskUUIDStr)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Invalid task UUID: %s", taskUUIDStr))
			continue
		}

		if err := taskDomain.ClearGitHubIssueFieldsFromTask(ctx, taskUUID); err != nil {
			errors = append(errors, fmt.Sprintf("Failed to clear Postgres fields for %s: %v", taskUUIDStr, err))
			continue
		}

		dgTask := &dgraphStruct.DgraphTask{
			Uid:               "uid(task)",
			Uuid:              taskUUIDStr,
			GitHubIssueNumber: nil,
			GitHubIssueURL:    nil,
			GitHubPRNumber:    nil,
			GitHubPRURL:       nil,
			GitHubBranch:      nil,
		}
		_, err = taskDomain.CreateOrUpdateDgraphTask(ctx, dgTask)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Failed to clear Dgraph fields for %s: %v", taskUUIDStr, err))
			continue
		}
		successCount++
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           fmt.Sprintf("Unlinked %d/%d tasks", successCount, len(input.TaskUUIDs)),
		"success_count": successCount,
		"total_count":   len(input.TaskUUIDs),
		"errors":        errors,
	})
}

// BulkLinkTasksToGitHub links multiple tasks to GitHub issues.
func BulkLinkTasksToGitHub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		Links []struct {
			TaskUUID    string `json:"task_uuid"`
			IssueNumber int    `json:"issue_number"`
			IssueURL    string `json:"issue_url"`
		} `json:"links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	if len(input.Links) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "No links provided"})
		return
	}

	if len(input.Links) > 100 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Cannot process more than 100 links at once"})
		return
	}

	var successCount int
	var errors []string

	for _, link := range input.Links {
		taskUUID, err := uuid.Parse(link.TaskUUID)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Invalid task UUID: %s", link.TaskUUID))
			continue
		}

		if link.IssueNumber <= 0 || link.IssueURL == "" {
			errors = append(errors, fmt.Sprintf("Invalid issue data for task %s", link.TaskUUID))
			continue
		}

		err = taskDomain.SetGitHubIssueFieldsOnTask(ctx, taskUUID, link.IssueNumber, link.IssueURL)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Failed to link task %s: %v", link.TaskUUID, err))
			continue
		}
		successCount++
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           fmt.Sprintf("Linked %d/%d tasks", successCount, len(input.Links)),
		"success_count": successCount,
		"total_count":   len(input.Links),
		"errors":        errors,
	})
}

// CreateGitHubPRForTask creates a draft PR on GitHub for a task.
func CreateGitHubPRForTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid task UUID",
		})
		return
	}

	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse request body",
		})
		return
	}

	// Get task branch (use "0x1" as userUid to avoid Dgraph "ID can't be empty" in server ops)
	dgraphTaskInfo, err := business.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), taskUUIDStr, "0x1")
	if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.GitHubBranch == nil || *dgraphTaskInfo.GitHubBranch == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Task has no GitHub branch. Create a branch first.",
		})
		return
	}

	pr, err := githubBusiness.CreatePullRequestForTask(ctx, taskUUID, input.Title, input.Body, *dgraphTaskInfo.GitHubBranch)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CreateGitHubPRForTask Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": err.Error(),
		})
		return
	}

	// Update task with PR info
	_ = taskDomain.UpdateTaskPRInfo(ctx, taskUUID, pr.Number, pr.HTMLURL)

	// Create activity
	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
		nil, nil, nil, nil, &pr.HTMLURL, nil)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "Pull request created",
		"pr_url":    pr.HTMLURL,
		"pr_number": pr.Number,
	})
}

// RetryGitHubSync re-enqueues a GitHub sync for a task.
func RetryGitHubSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid task UUID",
		})
		return
	}

	// Get current task info to determine what to sync (use "0x1" as userUid for server ops)
	dgraphTaskInfo, err := business.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), taskUUIDStr, "0x1")
	if err != nil || dgraphTaskInfo == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Task not found",
		})
		return
	}

	// Nothing to retry if the task was never linked. Without this the endpoint
	// answers "pending", queues five items that cannot succeed, and leaves the
	// panel showing a failed GitHub sync on a task that has no GitHub anything.
	if !syncBusiness.TaskHasGitHubLink(ctx, taskUUID) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "This task is not linked to a GitHub issue or pull request.",
		})
		return
	}

	// Reset sync status to pending
	_ = business.SetGitHubSyncStatus(ctx, taskUUID, "pending", nil, 0)

	// Enqueue full task sync
	go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "status", map[string]interface{}{
		"status": dgraphTaskInfo.Status,
	})

	if dgraphTaskInfo.Label != nil && *dgraphTaskInfo.Label != "" {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "label", map[string]interface{}{
			"label": *dgraphTaskInfo.Label,
		})
	}

	if dgraphTaskInfo.Name != "" {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "name", map[string]interface{}{
			"name": dgraphTaskInfo.Name,
		})
	}

	if dgraphTaskInfo.Description != nil && *dgraphTaskInfo.Description != "" {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "description", map[string]interface{}{
			"description": *dgraphTaskInfo.Description,
		})
	}

	if dgraphTaskInfo.Assignee != nil {
		go syncBusiness.EnqueueGitHubSync(ctx, taskUUID, "assignee", map[string]interface{}{
			"assignee_uuid": dgraphTaskInfo.Assignee.Uuid,
		})
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Sync retry queued",
		"status": "success",
	})
}

// RefreshFromGitHub pulls the latest state from GitHub and overwrites the OneCamp task.
func RefreshFromGitHub(w http.ResponseWriter, r *http.Request) {
	taskUUIDStr := chi.URLParam(r, "task_uuid")
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid task UUID",
		})
		return
	}

	// Refresh never backfills historical comments by default (good UX: avoids flooding
	// the task with old comments). Pass ?backfill=true to also import missing comments.
	backfill := r.URL.Query().Get("backfill") == "true"

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.LogErrorWithContext(context.Background(), "controllers/RefreshFromGitHub panic recovered: %v", rec)
				// Ensure we don't leave status stuck at pending after a panic
				reason := "internal error during refresh"
				_ = business.SetGitHubSyncStatus(context.Background(), taskUUID, "failed", &reason, 1)
			}
		}()
		procCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := githubBusiness.RefreshFromGitHub(procCtx, taskUUID, backfill); err != nil {
			helpers.LogErrorWithContext(procCtx, "controllers/RefreshFromGitHub Failed: %v", err)
		}
	}()

	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{
		"msg":    "Refresh started",
		"status": "accepted",
	})
}
