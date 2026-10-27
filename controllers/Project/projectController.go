package controllers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"io"
	"net/http"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Project"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	business "github.com/akashc777/OneCamp/business/Project"
	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userProjectNotificationBusiness "github.com/akashc777/OneCamp/business/UserProjectNotification"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
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

	// A template can come whole, as a plan the AI drafted that hasn't been
	// saved: it is checked as a template file is.
	var body struct {
		adapter.CreateOrUpdateProjectInput
		Template *templateBusiness.Template `json:"template,omitempty"`
	}
	err := json.NewDecoder(io.LimitReader(r.Body, templateBusiness.MaxFileBytes)).Decode(&body)
	createProjectInfo := body.CreateOrUpdateProjectInput
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
			"msg": "Couldn't create the project. Try again.",
			"err": err,
		})
		return
	}

	// Who makes projects in a team is the workspace's "Who can make projects":
	// its admins, or everyone in it (Work Access).
	if !workAccess.CanMakeProjects(ctx, dgraphTeam) || len(createProjectInfo.TeamUuid) == 0 || len(createProjectInfo.Name) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	// A template is found before anything is made, so one that's gone is a
	// message, not a project without its tasks.
	var template *templateBusiness.Template
	if body.Template != nil {
		t, err := templateBusiness.Check(*body.Template)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
			return
		}
		template = &t
	} else if createProjectInfo.TemplateID != "" {
		t, err := templateBusiness.Get(ctx, createProjectInfo.TemplateID)
		if errors.Is(err, templateBusiness.ErrNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That template isn't there any more. Pick another, or start blank."})
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/CreateProject template err: %+v", err)
			helpers.WriteRetryLater(w, "Templates couldn't be read just now.")
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
		// A project's name is unique within its team (migration 320): another team's
		// project of the same name doesn't collide, so a collision is in this team, and
		// the message says nothing about what other teams call theirs.
		if helpers.IsUniqueViolation(err) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":    "This team already has a project named \"" + createProjectInfo.Name + "\".",
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
			"msg": "Couldn't remove the file. Try again.",
			"err": err,
		})
		return

	}

	if _, ok := projectaccess.Allow(w, r, dgraphProjectInfo, workAccess.NeedManage, "Only the project's admins remove its files."); !ok {
		return
	}

	err = business.RemoveAttachmentFromProject(ctx, attachmentInfo.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAttachmentFromTask Failed to delete attachment from project err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't remove the file. Try again.",
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

	if len(updateProjectInfo.Attachments) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose the files to add."})
		return
	}
	if _, ok := projectaccess.Allow(w, r, dgraphProject, workAccess.NeedAddFiles, ""); !ok {
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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/RemoveAdminRoleFromMember")
		return
	}
	manages, ok := managesPeople(w, r, dgraphProject)
	if !ok {
		return
	}
	if status, msg := memberChangeRefusal(dgraphProject, manages, removeAdmin, userInfo.UserDgraphInfo.Uuid, addProjectMemberInputInfo.UserUuid); status != 0 {
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	projectUUID := uuid.MustParse(dgraphProject.Uuid)
	if err := business.SetRole(ctx, userInfo.UserPostgresInfo.Id, projectUUID, dgraphProject.Uid, dgraphProject.Members[0], len(dgraphProject.Admins) > 0, workAccess.RoleMember); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RemoveAdminRoleFromMember Failed to remove admin role err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change their role.")
		return
	}

	business.RecordMemberChange(r, resourceEventBusiness.ProjectChanged, "project.member.role", "Changed someone's role in a project", dgraphProject.Uuid, dgraphProject.Members[0].Uuid, workAccess.RoleMember)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": doneWords[removeAdmin]})

}

func RemoveMemberFromProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addProjectMemberInputInfo adapter.AddOrRemoveProjectMemberInput
	if err := json.NewDecoder(r.Body).Decode(&addProjectMemberInputInfo); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	if _, err := uuid.Parse(addProjectMemberInputInfo.ProjectUuid); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return
	}
	if _, err := uuid.Parse(addProjectMemberInputInfo.UserUuid); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a person."})
		return
	}

	dgraphProject, err := business.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, addProjectMemberInputInfo.ProjectUuid, userInfo.UserDgraphInfo.Uid, addProjectMemberInputInfo.UserUuid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/RemoveMemberFromProject")
		return
	}

	manages, ok := managesPeople(w, r, dgraphProject)
	if !ok {
		return
	}
	if status, msg := memberChangeRefusal(dgraphProject, manages, takeOff, userInfo.UserDgraphInfo.Uuid, addProjectMemberInputInfo.UserUuid); status != 0 {
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	if err := business.RemoveMemberFromProject(ctx, dgraphProject.Members[0].Uid, dgraphProject.Members[0].Uuid, dgraphProject.Uid, dgraphProject.Uuid); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RemoveMemberFromProject Failed to remove member from project err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't take them off the project. Try again."})
		return
	}

	summary := "Took someone off a project"
	if strings.EqualFold(userInfo.UserDgraphInfo.Uuid, addProjectMemberInputInfo.UserUuid) {
		summary = "Left a project"
	}
	business.RecordMemberChange(r, resourceEventBusiness.ProjectMemberRemoved, "project.member.remove", summary, dgraphProject.Uuid, dgraphProject.Members[0].Uuid, workAccess.RoleNone)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": doneWords[takeOff]})

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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetDgraphProjectAttachmentList")
		return
	}

	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	// Who adds files and who takes them away (project_access), for the
	// Files tab to offer only that.
	decorate(ctx, &userInfo, dgraphProject)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got project attachment list!", "data": dgraphProject})

}

func GetDgraphProjectCombinedMemberInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	dgraphProject, err := business.GetDgraphProjectCombinedMemberInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)
	if err == nil && dgraphProject == nil {
		err = helpers.ErrNotFound
	}

	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetDgraphProjectCombinedMemberInfo")
		return
	}

	if !dgraphProject.ReaderIsIn() && (dgraphProject.Team == nil || dgraphProject.Team.IsAdmin == 0) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}
	// Each person's role, and what the reader may do (change them, for one).
	if err := business.FillRoles(ctx, dgraphProject); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDgraphProjectCombinedMemberInfo couldn't read the roles err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load the project's people.")
		return
	}
	if err := workAccess.Decorate(ctx, userInfo.UserDgraphInfo.Uuid, dgraphProject); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetDgraphProjectCombinedMemberInfo couldn't read the reader's role err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load the project's people.")
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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetDgraphProjectMemberList")
		return
	}

	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Project member list", "data": dgraphProject})

}

func ProjectInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	// No project by that id is a 404 and a failed read a 503 (it answered
	// 400 for both, which the app could only call a connection problem:
	// PTC-1). Someone outside the project is refused below, 403: the app says
	// "You can't see this project".
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/ProjectInfo")
		return
	}

	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	// What the reader may do in it (project_access), from the rule: the app
	// shows what they can do and never guesses.
	if err := workAccess.Decorate(ctx, userInfo.UserDgraphInfo.Uuid, dgraphProject); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ProjectInfo couldn't read the reader's role err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load the project.")
		return
	}

	var notificationType string
	notificationType, err = userProjectNotificationBusiness.GetNotificationTypeByUserIdAndProjectId(userInfo.UserDgraphInfo.Uuid, projectUUIDString)

	// A member with no choice saved hears about everything, the level a new
	// member gets; only a read that failed fails the page.
	if errors.Is(err, sql.ErrNoRows) {
		notificationType, err = "all", nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ProjectInfo Failed to get users notificationType err: %+v",
			err)
		helpers.WriteRetryLater(w, "Couldn't load the project.")
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
			"msg": "Couldn't rename the project. Try again.",
			"err": err,
		})
		return
	}

	if _, ok := projectaccess.Allow(w, r, dgraphProject, workAccess.NeedManage, ""); !ok {
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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/AddMemberToProject")
		return
	}
	manages, ok := managesPeople(w, r, dgraphProject)
	if !ok {
		return
	}
	if status, msg := memberChangeRefusal(dgraphProject, manages, addMember, userInfo.UserDgraphInfo.Uuid, addProjectMemberInputInfo.UserUuid); status != 0 {
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	if err := business.AddMemberToProject(ctx, projectUUID, dgraphProject.Team.Members[0].Uid, dgraphProject.Team.Members[0].Uuid); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AddMemberToProject Failed to add member to project err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't add them to the project.")
		return
	}

	business.RecordMemberChange(r, resourceEventBusiness.ProjectMemberAdded, "project.member.add", "Added someone to a project", dgraphProject.Uuid, dgraphProject.Team.Members[0].Uuid, workAccess.RoleMember)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": doneWords[addMember]})

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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/AddAdminMemberToProject")
		return
	}
	manages, ok := managesPeople(w, r, dgraphProject)
	if !ok {
		return
	}
	if status, msg := memberChangeRefusal(dgraphProject, manages, makeAdmin, userInfo.UserDgraphInfo.Uuid, addProjectMemberInputInfo.UserUuid); status != 0 {
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	if err := business.SetRole(ctx, userInfo.UserPostgresInfo.Id, projectUUID, dgraphProject.Uid, dgraphProject.Members[0], len(dgraphProject.Admins) > 0, workAccess.RoleAdmin); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AddAdminMemberToProject Failed to add admin member to project err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't make them an admin.")
		return
	}

	business.RecordMemberChange(r, resourceEventBusiness.ProjectChanged, "project.member.role", "Made someone an admin of a project", dgraphProject.Uuid, dgraphProject.Members[0].Uuid, workAccess.RoleAdmin)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": doneWords[makeAdmin]})

}

// managesPeople is whether the caller may change who is in p (read for them)
// and what each may do there, by Work Access's rule: its admins, and its
// team's admins. A role that can't be read is a 503 written here.
func managesPeople(w http.ResponseWriter, r *http.Request, p *dgraphStruct.DgraphProject) (manages, ok bool) {
	if p == nil {
		return false, true
	}
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	s, err := workAccess.Of(r.Context(), u.UserDgraphInfo.Uuid, p)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Project couldn't read the caller's role err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't check what you can do in this project.")
		return false, false
	}
	return s.Can(workAccess.NeedManagePeople), true
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
			"msg": "Couldn't archive the project. Try again.",
			"err": err,
		})
		return
	}

	// Its admins, and its team's admins (Work Access).
	if _, ok := projectaccess.Allow(w, r, dgraphProjectInfo, workAccess.NeedArchive, ""); !ok {
		return
	}

	// The demo's own projects stay for every visitor (business/DemoGuard).
	if demoGuard.KeepsFromVisitor(userInfo.UserPostgresInfo.EmailID, dgraphProjectInfo.CreatedAt) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoSeededMsg})
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
			"msg": "Couldn't restore the project. Try again.",
			"err": err,
		})
		return
	}

	// Its admins, and its team's admins (Work Access).
	if _, ok := projectaccess.Allow(w, r, dgraphProjectInfo, workAccess.NeedArchive, ""); !ok {
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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetProjectTaskList")
		return
	}

	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
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

	// One builder for every task list (domain/Task.TaskListFilter, PR1).
	params := make([]taskDomain.TaskListParam, 0, len(filtersParam))
	for _, p := range filtersParam {
		params = append(params, taskDomain.TaskListParam{ID: p.Id, Value: p.Value})
	}
	listFilter := taskDomain.TaskListFilter{Params: params, Now: time.Now(), Status: taskStatusBusiness.FilterClause}
	listFilter.Side = func(id string, values []string) (string, bool) {
		return sideFilterClause(ctx, projectUUIDString, id, values)
	}
	if len(taskSearchStr) != 0 {
		listFilter.Search = taskSearchStr[0]
	}
	filterQuery, err := listFilter.Clause()
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search string is too long or invalid"})
		return
	}

	// One mapping from the sorts the list offers to the graph's: no key a
	// request names is passed to the graph as it came.
	sortingQuery, err := dgraphquery.TaskListOrder(queryParams.Get("sorting"))
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

	dgraphProjectWithTasks, err := business.GetDgraphProjectTaskList(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid, filterQuery, sortingQuery, pageSize, pageIndex, getAll)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskList Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the project's tasks.",
			"err": err,
		})
		return

	}

	// Overlay ephemeral GitHub metadata from PostgreSQL onto Dgraph tasks
	if dgraphProjectWithTasks != nil && len(dgraphProjectWithTasks.Tasks) > 0 {
		// GitHub's facts are already on them (business.GetDgraphProjectTaskList; PR1, #24).
		_ = taskFieldBusiness.MergeFieldValues(ctx, dgraphProjectWithTasks.Tasks)
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphProjectWithTasks.TaskCount + uint64(pageSize) - 1) / uint64(pageSize)
	}
	decorate(ctx, &userInfo, dgraphProjectWithTasks)

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
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetProjectTaskListForKanban")
		return
	}

	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	filterQuery, err := kanbanFilterQuery(ctx, projectUUIDString, r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse filters query param",
			"err": err,
		})
		return
	}

	dgraphProjectWithTasks, err := business.GetDgraphProjectTaskListForKanban(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid, filterQuery, helpers.OpenLimit(r.URL.Query()), helpers.ClosedLimit(r.URL.Query()))

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectTaskListForKanban Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the project's board.",
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
		// GitHub's facts are already on them (business.GetDgraphProjectTaskListForKanban; PR1, #24).
		_ = taskFieldBusiness.MergeFieldValues(ctx, allTasks)
	}
	decorate(ctx, &userInfo, dgraphProjectWithTasks)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Sucessful got task list",
		"data": dgraphProjectWithTasks,
	})
}

// kanbanFilterQuery is the board's filters (?filters=) as one DQL clause, for
// the board and for a page of one of its columns alike, so both read the same
// cards.
func kanbanFilterQuery(ctx context.Context, projectUUIDString string, r *http.Request) (string, error) {
	queryParams := r.URL.Query()

	filtersParamString := queryParams["filters"]
	var filtersParam []adapter.FilterParam

	if len(filtersParamString) != 0 {
		err := json.Unmarshal([]byte(filtersParamString[0]), &filtersParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetProjectTaskListForKanban Failed to parse filters query param err: %+v",
				err)

			return "", err

		}

	}

	// One builder for every task list (domain/Task.TaskListFilter, PR1).
	params := make([]taskDomain.TaskListParam, 0, len(filtersParam))
	for _, p := range filtersParam {
		params = append(params, taskDomain.TaskListParam{ID: p.Id, Value: p.Value})
	}
	listFilter := taskDomain.TaskListFilter{Params: params, Now: time.Now(), Status: taskStatusBusiness.FilterClause}
	listFilter.Side = func(id string, values []string) (string, bool) {
		return sideFilterClause(ctx, projectUUIDString, id, values)
	}
	return listFilter.Clause()
}

// decorate sets what the reader may do in the project they read
// (project_access, Work Access), for a list or a board to offer only that.
// Without it the app keeps to the project's admins changing its tasks, so a
// failed read of roles is said in the log and the list still shows.
func decorate(ctx context.Context, u *userModels.UserInfo, p *dgraphStruct.DgraphProject) {
	if p == nil {
		return
	}
	if err := workAccess.Decorate(ctx, u.UserDgraphInfo.Uuid, p); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Project couldn't read what the reader may do in project %s err: %+v", p.Uuid, err)
	}
}

// sideFilterClause is the clause for a filter on what Postgres keeps beside a
// project's tasks: their cycle (business/Cycle) or a custom field's value
// (business/TaskField). ok is false for any other filter.
func sideFilterClause(ctx context.Context, projectUUID, id string, values []string) (clause string, ok bool) {
	var err error
	switch {
	case id == "task_cycle":
		clause, err = cycleBusiness.FilterClause(projectUUID, values)
	case strings.HasPrefix(id, taskFieldBusiness.FilterPrefix):
		var project uuid.UUID
		if project, err = uuid.Parse(projectUUID); err == nil {
			clause, err = taskFieldBusiness.FilterClause(ctx, project, id, values)
		}
	default:
		return "", false
	}
	// A filter that couldn't be read narrows to nothing: an empty list says
	// something went wrong, where the whole list would pass for a filtered one.
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Project %s filter err: %+v", id, err)
		return `eq(task_uuid, "00000000-0000-0000-0000-000000000000")`, true
	}
	return clause, true
}

// GetProjectBoardColumn answers GET /project/boardColumn/{project_uuid}
// ?status=&after=&limit=&filters=: the next cards of one open column of the
// project's board, after the card whose uuid is after (DG4b). A reader of
// the board may read its columns. 409 when after is no longer in the column
// (the web re-reads the board), 400 for a closed or unknown column.
func GetProjectBoardColumn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	projectUUIDString := chi.URLParam(r, "project_uuid")
	if _, err := uuid.Parse(projectUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Not a project id"})
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	after := q.Get("after")
	if status == "" || (after != "" && uuid.Validate(after) != nil) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A column page needs a status, and after must be a card's id"})
		return
	}
	dgraphProject, err := business.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/GetProjectBoardColumn")
		return
	}
	if !dgraphProject.ReaderIsIn() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	filterQuery, err := kanbanFilterQuery(ctx, projectUUIDString, r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse filters query param"})
		return
	}
	page, err := business.GetProjectBoardColumnPage(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid, filterQuery, status, after,
		helpers.LimitParam(q, business.BoardColumnMax, business.BoardColumnMax))
	switch {
	case errors.Is(err, taskrank.ErrNoSuchCard):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "The board changed; read it again"})
		return
	case errors.Is(err, taskrank.ErrClosedColumn), errors.Is(err, taskrank.ErrUnknownColumn):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	case err != nil:
		helpers.WriteReadFailure(w, r, err, "board column", "controllers/GetProjectBoardColumn")
		return
	}
	_ = taskFieldBusiness.MergeFieldValues(ctx, page.Cards)
	// What the reader may do with each card, as the board says it.
	view := &dgraphStruct.DgraphProject{Uuid: projectUUIDString, TasksTodo: page.Cards}
	decorate(ctx, &userInfo, view)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": page})
}
