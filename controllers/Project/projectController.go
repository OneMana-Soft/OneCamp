package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Project"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	business "github.com/akashc777/OneCamp/business/Project"
	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userProjectNotificationBusiness "github.com/akashc777/OneCamp/business/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func CreateProject(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createProjectInfo adapter.CreateOrUpdateProjectInput
	err := json.NewDecoder(r.Body).Decode(&createProjectInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	dgraphTeam, err := teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, createProjectInfo.TeamUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateProject Failed to team dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to team dgraph info",
			"err": err,
		})
		return
	}

	if dgraphTeam.IsAdmin == 0 || len(createProjectInfo.TeamUuid) == 0 || len(createProjectInfo.Name) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	// A template is found before anything is made, so one that's gone is a
	// message, not a project without its tasks.
	var template *templateBusiness.Template
	if createProjectInfo.TemplateID != "" {
		t, err := templateBusiness.Get(ctx, createProjectInfo.TemplateID)
		if errors.Is(err, templateBusiness.ErrNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That template isn't there any more. Pick another, or start blank."})
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/CreateProject template err: %+v", err)
			helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Templates couldn't be read just now. Try again in a moment."})
			return
		}
		template = &t
	}

	teamUUID, err := uuid.Parse(dgraphTeam.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateProject Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.CreateProject(ctx, createProjectInfo.Name, userInfo.UserDgraphInfo.Uid, userInfo.UserPostgresInfo.Id, dgraphTeam, teamUUID, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateProject Failed to create project err: %+v",
			err)
		// A duplicate name is the one failure here the user can act on, so it gets its own
		// status and a message naming the project. The client renders msg verbatim.
		//
		// Note projects carries TWO unique constraints — project_name alone, and
		// (project_name, team_id) — so a name taken by another team collides too. The message
		// says a project with that name exists rather than claiming it is in this team.
		if helpers.IsUniqueViolation(err) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":    "A project named \"" + createProjectInfo.Name + "\" already exists.",
				"status": "failed",
			})
			return
		}

		// Anything else is ours: 500, not 400. The error object stays out of the body — a
		// *pq.Error marshals its exported fields, so returning it hands the client the
		// constraint name, the table and column, and Postgres' own source location.
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "Could not create the project. Please try again.",
			"status": "failed",
		})
		return
	}

	if template == nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created project!", "data": dgraphProject})
		return
	}
	start := templateBusiness.StartOn(createProjectInfo.StartDate, createProjectInfo.TZ, createProjectInfo.SkipWeekends)
	applied := templateBusiness.ApplyTo(ctx, *template, dgraphProject.Uuid, &userInfo, start)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created project!", "data": struct {
		*dgraphStruct.DgraphProject
		Template templateBusiness.Applied `json:"template"`
	}{dgraphProject, applied}})
}

func GetDgraphProjectListByAdminDgraphUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphProject, err := business.GetDgraphProjectListByAdminDgraphUID(ctx, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphProjectListByAdminDgraphUID Failed to get dgraph project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info req",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated project!", "data": dgraphProject})

}

func RemoveAttachmentFromProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var removeProjectAttachmentInputInfo adapter.RemoveProjectAttachmentInput
	err := json.NewDecoder(r.Body).Decode(&removeProjectAttachmentInputInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminRoleFromMember Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(removeProjectAttachmentInputInfo.AttachmentObjKey) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	attachmentInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, removeProjectAttachmentInputInfo.AttachmentObjKey, postgressStruct.ATTACHMENT_SRC_PROJECT)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromProject Failed to get attachment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachment info",
			"err": err,
		})
		return

	}

	dgraphProjectInfo, err := business.GetBasicDgraphProjectInfo(ctx, attachmentInfo.SrcValue, userInfo.UserDgraphInfo.Uid)

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

	if dgraphProjectInfo.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.RemoveAttachmentFromProject(ctx, attachmentInfo.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to delete attachment from project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project dgraph info",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": attachmentInfo.Uuid})

}

func AddAttachmentToProjectDgraph(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var updateProjectInfo adapter.CreateOrUpdateProjectInput
	err := json.NewDecoder(r.Body).Decode(&updateProjectInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToProjectDgraph Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	projectUUID, err := uuid.Parse(updateProjectInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToProjectDgraph Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, updateProjectInfo.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToProjectDgraph Failed to get project info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectAdmin == 0 || len(updateProjectInfo.Attachments) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	err = business.AddAttachmentToProjectDgraph(ctx, projectUUID, &updateProjectInfo, &userInfo.UserDgraphInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAttachmentToProjectDgraph Failed to add attachmentss to project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add attachmentss to project",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added attachment to project!"})

}

func RemoveAdminRoleFromMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectMemberInputInfo adapter.AddOrRemoveProjectMemberInput
	err := json.NewDecoder(r.Body).Decode(&addProjectMemberInputInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminRoleFromMember Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	_, err = uuid.Parse(addProjectMemberInputInfo.ProjectUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, addProjectMemberInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid, addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminRoleFromMember Failed to get basic dgraph project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get basic dgraph project info",
			"err": err,
		})
		return

	}

	if (dgraphProject.IsProjectAdmin == 0 && dgraphProject.Team.IsAdmin == 0) || len(dgraphProject.Members) == 0 || dgraphProject.CreatedBy.Uuid == addProjectMemberInputInfo.UserUuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	err = business.RemoveAdminMemberFromProject(ctx, dgraphProject.Members[0].Uid, dgraphProject.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminRoleFromMember Failed to remove admin role err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove admin role",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "member removed from project!"})

}

func RemoveMemberFromProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectMemberInputInfo adapter.AddOrRemoveProjectMemberInput
	err := json.NewDecoder(r.Body).Decode(&addProjectMemberInputInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	_, err = uuid.Parse(addProjectMemberInputInfo.ProjectUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, addProjectMemberInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid, addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromProject Failed to get basic dgraph project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get basic dgraph project info",
			"err": err,
		})
		return

	}

	if (len(dgraphProject.Members) == 0 && dgraphProject.Team.IsAdmin == 0) || dgraphProject.CreatedBy.Uid == dgraphProject.Members[0].Uid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	err = business.RemoveMemberFromProject(ctx, dgraphProject.Members[0].Uid, dgraphProject.Members[0].Uuid, dgraphProject.Uid, dgraphProject.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromProject Failed to remove member from project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove member from project",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "member removed from project!"})

}

func GetDgraphProjectAttachmentList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetDgraphProjectAttachmentsInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphProjectAttachmentList Failed to get basic dgraph attachment list info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get basic dgraph attachment list",
			"err": err,
		})
		return

	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got project attachment list!", "data": dgraphProject})

}

func GetDgraphProjectCombinedMemberInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	dgraphProject, err := business.GetDgraphProjectCombinedMemberInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphProjectCombinedMemberInfo Failed to get dgraph member info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph member info",
			"err": err,
		})
		return

	}

	if dgraphProject.IsProjectMember == 0 && dgraphProject.Team.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got project member list!", "data": dgraphProject})

}

func GetDgraphProjectMemberList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetDgraphProjectMemberInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphProjectMemberList Failed to get basic dgraoh project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info",
			"err": err,
		})
		return

	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Project member list", "data": dgraphProject})

}

func ProjectInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ProjectInfo Failed to get basic dgraoh project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info",
			"err": err,
		})
		return

	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return

	}

	var notificationType string
	notificationType, err = userProjectNotificationBusiness.GetNotificationTypeByUserIdAndProjectId(userInfo.UserDgraphInfo.Uuid, projectUUIDString)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ProjectInfo Failed to get users notificationType err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users notificationType",
			"err": err,
		})
		return
	}
	dgraphProject.NotificationType = notificationType

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got project info!", "data": dgraphProject})

}

func UpdateProjectName(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var updateProjectInfo adapter.CreateOrUpdateProjectInput
	err := json.NewDecoder(r.Body).Decode(&updateProjectInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateProjectName Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, updateProjectInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateProjectName Failed to get dgraph project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph project",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	projectUUID, err := uuid.Parse(updateProjectInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateProjectName Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	err = business.UpdateProjectName(ctx, updateProjectInfo.Name, projectUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateProjectName Failed to update project name err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update project name",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated project!"})

}

func AddMemberToProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectMemberInputInfo adapter.AddOrRemoveProjectMemberInput
	err := json.NewDecoder(r.Body).Decode(&addProjectMemberInputInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	projectUUID, err := uuid.Parse(addProjectMemberInputInfo.ProjectUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToProject Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfoWithGivenMemberAndTeamUUID(ctx, addProjectMemberInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid, addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToProject Failed to get project info from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info from dgraph",
			"err": err,
		})
		return

	}

	if (dgraphProject.IsProjectAdmin == 0 && dgraphProject.Team.IsAdmin == 0) || len(dgraphProject.Team.Members) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	if len(dgraphProject.Team.Members) > 0 && dgraphProject.Team.Members[0].IsExternal {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Cannot add external users to projects",
		})
		return
	}

	err = business.AddMemberToProject(ctx, projectUUID, dgraphProject.Team.Members[0].Uid, dgraphProject.Team.Members[0].Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToProject Failed to add member to project err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member to project",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added member to project!"})

}

func AddAdminMemberToProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectMemberInputInfo adapter.AddOrRemoveProjectMemberInput
	err := json.NewDecoder(r.Body).Decode(&addProjectMemberInputInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(addProjectMemberInputInfo.UserUuid) == 0 || len(addProjectMemberInputInfo.ProjectUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	projectUUID, err := uuid.Parse(addProjectMemberInputInfo.ProjectUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToProject Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(addProjectMemberInputInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToProject Failed to parse userUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, addProjectMemberInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid, addProjectMemberInputInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToProject Failed to get project info from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get project info from dgraph",
			"err": err,
		})
		return

	}

	if (dgraphProject.IsProjectAdmin == 0 && dgraphProject.Team.IsAdmin == 0) || len(dgraphProject.Members) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.AddAdminMemberToProject(ctx, projectUUID, dgraphProject.Members[0].Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToProject Failed to add admin member to project err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add admin member to project",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added member admin to project!"})

}

func ArchiveProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectInputInfo adapter.AddOrRemoveProjectInput
	err := json.NewDecoder(r.Body).Decode(&addProjectInputInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	projectUUID, err := uuid.Parse(addProjectInputInfo.ProjectUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse project string to uuid",
			"err": err,
		})
		return
	}

	dgraphProjectInfo, err := business.GetDgraphProjectInfoAndTeamAdminFlag(ctx, addProjectInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveProject Failed to get dgraph project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph project info",
			"err": err,
		})
		return
	}

	if dgraphProjectInfo.Team.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.ArchiveProjectByProjectUUID(ctx, projectUUID, dgraphProjectInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveProject Failed to archive project err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to archive project",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Archived project!"})

}

func UnArchiveProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectInputInfo adapter.AddOrRemoveProjectInput
	err := json.NewDecoder(r.Body).Decode(&addProjectInputInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveProject Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	projectUUID, err := uuid.Parse(addProjectInputInfo.ProjectUuid)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse project string to uuid",
			"err": err,
		})
		return
	}

	dgraphProjectInfo, err := business.GetDgraphProjectInfoAndTeamAdminFlag(ctx, addProjectInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveProject Failed to get dgraph project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph project info",
			"err": err,
		})
		return
	}

	if dgraphProjectInfo.Team.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.UnArchiveProjectUUID(ctx, projectUUID, dgraphProjectInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveProject Failed to archive project err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to un-archive project",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "UnArchived project!"})

}

func GetProjectTaskList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskList Failed to get dgraph project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph project",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	queryParams := r.URL.Query()

	filtersParamString := queryParams["filters"]
	var filtersParam []adapter.FilterParam

	if len(filtersParamString) != 0 {
		err := json.Unmarshal([]byte(filtersParamString[0]), &filtersParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetProjectTaskList Failed to parse filters query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}

	}

	taskSearchStr := queryParams["taskSearchString"]

	getAll, pageSize, pageIndex, err := helpers.ListPaging(queryParams)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	var filterStrings []string
	currentTime := time.Now()

	if len(taskSearchStr) != 0 {
		safe, sErr := dgraphquery.SanitizeSearchTerm(taskSearchStr[0])
		if sErr != nil {
			if !errors.Is(sErr, dgraphquery.ErrEmpty) {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "search string is too long or invalid",
				})
				return
			}
		} else {
			filterStrings = append(filterStrings, fmt.Sprintf(`regexp(task_name,  /.*%s.*/i)`, safe))
		}
	}

	for _, param := range filtersParam {

		// Refuse unrecognised column names so a hostile body can't
		// inject DQL via the `id` field.
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}

		var filterValue string
		switch v := param.Value.(type) {
		case string:
			if param.Id == "task_name" {
				safe, sErr := dgraphquery.SanitizeSearchTerm(v)
				if sErr == nil {
					filterValue = fmt.Sprintf(`regexp(%s,  /.*%s.*/i)`, param.Id, safe)
				}
			}
			if param.Id == "overdue" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, currentTime.Format(time.RFC3339Nano))
			}
			if param.Id == "upcoming" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND gt(task_start_date, "%s")`, currentTime.Format(time.RFC3339Nano))
			}
		case []interface{}:
			strValues := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && dgraphquery.IsAllowedFilterValue(s) {
					strValues = append(strValues, s)
				}
			}
			if param.Id == "task_priority" {
				filterValue = fmt.Sprintf(`anyofterms(%s,"%s")`, param.Id, strings.Join(strValues, " "))
			}
			// Built-in and custom statuses; see business/TaskStatus.
			if param.Id == "task_status" {
				filterValue = taskStatusBusiness.FilterClause(strValues)
			}
			if param.Id == "task_assignee_name" {
				filterValue = fmt.Sprintf(`uid_in(task_assignee, [%s])`, strings.Join(strValues, ", "))
			}
			// Cycles live in Postgres; see business/Cycle.
			if param.Id == "task_cycle" {
				clause, cErr := cycleBusiness.FilterClause(projectUUIDString, strValues)
				if cErr != nil {
					helpers.LogErrorWithContext(ctx, "controllers/Project cycle filter err: %+v", cErr)
				}
				filterValue = clause

			}

		}

		if len(filterValue) > 0 {
			filterStrings = append(filterStrings, filterValue)
		}
	}

	filterQuery := strings.Join(filterStrings, " AND ")

	sortParamString := queryParams["sorting"]

	var sortParam []adapter.SortingParam

	if len(sortParamString) != 0 {
		err := json.Unmarshal([]byte(sortParamString[0]), &sortParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetProjectTaskList Failed to parse sorting query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse sorting query param",
				"err": err,
			})
			return

		}

	}

	sortingQuery := ""
	for _, param := range sortParam {
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}
		switch param.Desc {
		case true:
			sortingQuery = fmt.Sprintf("orderdesc: %s", param.Id)
		case false:

			sortingQuery = fmt.Sprintf("orderasc: %s", param.Id)

		}

	}

	if len(sortingQuery) == 0 {
		sortingQuery = "orderdesc: task_created_at"
	}

	dgraphProjectWithTasks, err := business.GetDgraphProjectTaskList(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid, filterQuery, sortingQuery, pageSize, pageIndex, getAll)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskList Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph user task",
			"err": err,
		})
		return

	}

	// Overlay ephemeral GitHub metadata from PostgreSQL onto Dgraph tasks
	if dgraphProjectWithTasks != nil && len(dgraphProjectWithTasks.Tasks) > 0 {
		_ = taskBusiness.MergeGitHubMetaIntoTasks(ctx, dgraphProjectWithTasks.Tasks)
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphProjectWithTasks.TaskCount + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "Sucessful got task list",
		"data":      dgraphProjectWithTasks,
		"pageCount": pageCount,
	})
}

func GetProjectTaskListForKanban(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid err",
			"err": err,
		})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskListForKanban Failed to get dgraph project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph project",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	queryParams := r.URL.Query()

	filtersParamString := queryParams["filters"]
	var filtersParam []adapter.FilterParam

	if len(filtersParamString) != 0 {
		err := json.Unmarshal([]byte(filtersParamString[0]), &filtersParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetProjectTaskListForKanban Failed to parse filters query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}

	}

	var filterStrings []string
	currentTime := time.Now()

	for _, param := range filtersParam {

		// Refuse unrecognised column names.
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}

		var filterValue string
		switch v := param.Value.(type) {
		case string:
			if param.Id == "task_name" {
				safe, sErr := dgraphquery.SanitizeSearchTerm(v)
				if sErr == nil {
					filterValue = fmt.Sprintf(`regexp(%s,  /.*%s.*/i)`, param.Id, safe)
				}
			}
			if param.Id == "overdue" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, currentTime.Format(time.RFC3339Nano))
			}
			if param.Id == "upcoming" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND gt(task_start_date, "%s")`, currentTime.Format(time.RFC3339Nano))
			}
		case []interface{}:
			strValues := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && dgraphquery.IsAllowedFilterValue(s) {
					strValues = append(strValues, s)
				}
			}
			if param.Id == "task_priority" {
				filterValue = fmt.Sprintf(`anyofterms(%s,"%s")`, param.Id, strings.Join(strValues, " "))
			}
			// Built-in and custom statuses; see business/TaskStatus.
			if param.Id == "task_status" {
				filterValue = taskStatusBusiness.FilterClause(strValues)
			}
			if param.Id == "task_assignee_name" {
				filterValue = fmt.Sprintf(`uid_in(task_assignee, [%s])`, strings.Join(strValues, ", "))
			}
			// Cycles live in Postgres; see business/Cycle.
			if param.Id == "task_cycle" {
				clause, cErr := cycleBusiness.FilterClause(projectUUIDString, strValues)
				if cErr != nil {
					helpers.LogErrorWithContext(ctx, "controllers/Project cycle filter err: %+v", cErr)
				}
				filterValue = clause

			}

		}

		if len(filterValue) > 0 {
			filterStrings = append(filterStrings, filterValue)
		}
	}

	filterQuery := strings.Join(filterStrings, " AND ")

	sortParamString := queryParams["sorting"]

	var sortParam []adapter.SortingParam

	if len(sortParamString) != 0 {
		err := json.Unmarshal([]byte(sortParamString[0]), &sortParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetProjectTaskListForKanban Failed to parse sorting query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse sorting query param",
				"err": err,
			})
			return

		}

	}

	sortingQuery := ""
	for _, param := range sortParam {
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}
		switch param.Desc {
		case true:
			sortingQuery = fmt.Sprintf("orderdesc: %s", param.Id)
		case false:

			sortingQuery = fmt.Sprintf("orderasc: %s", param.Id)

		}

	}

	if len(sortingQuery) == 0 {
		sortingQuery = "orderdesc: task_created_at"
	}

	dgraphProjectWithTasks, err := business.GetDgraphProjectTaskListForKanban(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid, filterQuery, helpers.ClosedLimit(r.URL.Query()))

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskListForKanban Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph user task",
			"err": err,
		})
		return

	}

	// Overlay ephemeral GitHub metadata from PostgreSQL onto Dgraph tasks for all kanban columns
	if dgraphProjectWithTasks != nil {
		var allTasks []*dgraphStruct.DgraphTask
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksTodo...)
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksInProgresss...)
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksBacklog...)
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksInReview...)
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksCanceled...)
		allTasks = append(allTasks, dgraphProjectWithTasks.TasksDone...)
		_ = taskBusiness.MergeGitHubMetaIntoTasks(ctx, allTasks)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Sucessful got task list",
		"data": dgraphProjectWithTasks,
	})
}
