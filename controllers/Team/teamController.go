package controllers

import (
	"encoding/json"
	"net/http"
	"strconv"

	adapter "github.com/akashc777/OneCamp/adapter/Team"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	business "github.com/akashc777/OneCamp/business/Team"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func CreateTeam(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var createTeamInfo adapter.CreateOrUpdateTeamInput
	err := json.NewDecoder(r.Body).Decode(&createTeamInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only workspace admins can create teams."})
		return
	}
	if !helpers.IsValidName(createTeamInfo.Name) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": helpers.NameRuleMessage})
		return
	}

	exists, err := business.CheckIfTeamExistByTeamName(ctx, createTeamInfo.Name)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTeam Failed to check if team name exist err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to check if team name exist",
			"err": err,
		})
		return
	}

	if exists {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "team name already exists",
		})
		return

	}

	dgraphTeam, err := business.CreateTeam(ctx, createTeamInfo.Name, &userInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateTeam Failed to create team err: %+v",
			err)

		// A duplicate name is the one failure here the user can act on, so it gets its own
		// status and a message naming the team. The client renders msg verbatim.
		if helpers.IsUniqueViolation(err) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":    "A team named \"" + createTeamInfo.Name + "\" already exists.",
				"status": "failed",
			})
			return
		}

		// Anything else is ours: 500, not 400. The error object stays out of the body — a
		// *pq.Error marshals its exported fields, so returning it hands the client the
		// constraint name, the table and column, and Postgres' own source location.
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "Could not create the team. Please try again.",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created team!", "data": dgraphTeam})

}

func CheckIfTeamNameExist(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	teamName := r.URL.Query()["team_name"]

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only workspace admins can create teams."})
		return
	}
	if len(teamName) == 0 || !helpers.IsValidName(teamName[0]) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": helpers.NameRuleMessage})
		return
	}

	exists, err := business.CheckIfTeamExistByTeamName(ctx, teamName[0])

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CheckIfTeamNameExist Failed to check if team name exist err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to check if team name exist",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"exists": exists})
}

func UpdateTeamName(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputTeamInfo adapter.CreateOrUpdateTeamInput
	err := json.NewDecoder(r.Body).Decode(&inputTeamInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTeamName Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(inputTeamInfo.Uuid) == 0 || len(inputTeamInfo.Name) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which team, and its new name."})
		return
	}

	teamUUID, err := uuid.Parse(inputTeamInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTeamName Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeamInfo, err := business.GetBasicDgraphTeamInfoByUUID(ctx, inputTeamInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTeamName Failed to get dgraph team info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return

	}

	if dgraphTeamInfo.IsAdmin == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.UpdateTeamName(ctx, inputTeamInfo.Name, teamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateTeamName Failed to update team name err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update team name",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated team name!"})

}

func AddMemberToTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addMemberInfo adapter.AddOrRemoveTeamMemberInput
	err := json.NewDecoder(r.Body).Decode(&addMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	teamUUID, err := uuid.Parse(addMemberInfo.TeamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(addMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to parse userUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeamInfo, err := business.GetBasicDgraphTeamInfoByUUID(ctx, addMemberInfo.TeamUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to get dgraph team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return
	}

	if dgraphTeamInfo.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	memberInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, addMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to get user member info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user member info",
			"err": err,
		})
		return
	}

	if memberInfo == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That person isn't in OneCamp."})
		return
	}

	if memberInfo.IsExternal {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Cannot add external users to teams",
		})
		return
	}

	err = business.AddMemberToTeam(ctx, teamUUID, memberInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddMemberToTeam Failed to add member to team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member to team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added member to team!"})

}

func AddAdminMemberToTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var addMemberInfo adapter.AddOrRemoveTeamMemberInput
	err := json.NewDecoder(r.Body).Decode(&addMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	teamUUID, err := uuid.Parse(addMemberInfo.TeamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToTeam Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeamInfo, err := business.GetBasicDgraphTeamInfoAndMemberInfoByUUID(ctx, addMemberInfo.TeamUUID, userInfo.UserDgraphInfo.Uid, addMemberInfo.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToTeam Failed to get team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get team info",
			"err": err,
		})
		return
	}

	if (dgraphTeamInfo.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin) || len(dgraphTeamInfo.Members) == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.AddAdminMemberToTeam(ctx, teamUUID, dgraphTeamInfo.Members[0].Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddAdminMemberToTeam Failed to add admin member to team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member to team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added admin member to team!"})

}

func RemoveMemberFromTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var removeMemberInfo adapter.AddOrRemoveTeamMemberInput
	err := json.NewDecoder(r.Body).Decode(&removeMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	_, err = uuid.Parse(removeMemberInfo.TeamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromTeam Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	if removeMemberInfo.UserUuid == userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "You can't change your own place in the team here."})
		return
	}

	_, err = uuid.Parse(removeMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromTeam Failed to parse userUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeamInfo, err := business.GetBasicDgraphTeamInfoAndMemberInfoByUUID(ctx, removeMemberInfo.TeamUUID, userInfo.UserDgraphInfo.Uid, removeMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromTeam Failed to get dgraph team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return
	}

	if (dgraphTeamInfo.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin) || len(dgraphTeamInfo.Members) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.RemoveMemberFromTeam(ctx, dgraphTeamInfo, dgraphTeamInfo.Members[0].Uid, dgraphTeamInfo.Members[0].Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveMemberFromTeam Failed to remove member from team err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove member from team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed member from team!"})

}

func RemoveAdminMemberFromTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var removeMemberInfo adapter.AddOrRemoveTeamMemberInput
	err := json.NewDecoder(r.Body).Decode(&removeMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminMemberFromTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	_, err = uuid.Parse(removeMemberInfo.TeamUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminMemberFromTeam Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	_, err = uuid.Parse(removeMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminMemberFromTeam Failed to parse userUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID string to uuid",
			"err": err,
		})
		return
	}

	if removeMemberInfo.UserUuid == userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "You can't change your own place in the team here."})
		return
	}

	dgraphTeamInfo, err := business.GetBasicDgraphTeamInfoAndAdminMemberInfoByUUID(ctx, removeMemberInfo.TeamUUID, userInfo.UserDgraphInfo.Uid, removeMemberInfo.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminMemberFromTeam Failed to get team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get team info",
			"err": err,
		})
		return
	}

	if dgraphTeamInfo.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.RemoveAdminMemberFromTeam(ctx, dgraphTeamInfo.Uid, dgraphTeamInfo.Admins[0].Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdminMemberFromTeam Failed to remove member from team err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member to team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed member from team!"})

}

func GetDgraphTeamProjectListByTeamUUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	teamUUIDString := chi.URLParam(r, "team_uuid")

	_, err := uuid.Parse(teamUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamProjectListByTeamUUID Failed to parse team string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse team string to uuid",
			"err": err,
		})
		return
	}

	basicDgraphTeamInfo, err := business.GetBasicDgraphTeamInfoByUUID(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamProjectListByTeamUUID Failed to get dgraph team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return
	}

	if basicDgraphTeamInfo.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	var dgraphTeamInfo *dgraphStruct.DgraphTeam

	if basicDgraphTeamInfo.IsAdmin == 1 {
		dgraphTeamInfo, err = business.GetDgraphAllProjectListByTeamUUID(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphTeamProjectListByTeamUUID failed to get team admin project list err: %+v",
				err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse team string to uuid",
				"err": err,
			})
			return
		}
	}

	if dgraphTeamInfo == nil {
		dgraphTeamInfo, err = business.GetDgraphAllProjectListByTeamUUIDInWhichUserIsMember(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphTeamProjectListByTeamUUID failed to get team admin project list err: %+v",
				err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get team admin project list",
				"err": err,
			})
			return
		}
	}

	dgraphTeamInfo.IsAdmin = basicDgraphTeamInfo.IsAdmin

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got project list!", "data": dgraphTeamInfo})

}

func GetDgraphTeamMemberListByTeamUUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	teamUUIDString := chi.URLParam(r, "team_uuid")

	_, err := uuid.Parse(teamUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamMemberListByTeamUUID to parse team string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse team string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeamInfo, err := business.GetDgraphTeamMemberListByTeamUUID(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamMemberListByTeamUUID Failed to get dgraph team member list err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team member list",
			"err": err,
		})
		return
	}

	if dgraphTeamInfo.IsMember == 0 && !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got members list!", "data": dgraphTeamInfo})

}

func GetDgraphTeamListByUserDgraphUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphTeamList, err := business.GetDgraphTeamListByUserDgraphUID(ctx, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamListByUserDgraphUID Failed to get dgraph team list info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got team list!", "data": dgraphTeamList})

}

func GetDgraphTeamListByAdminDgraphUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphTeamList, err := business.GetDgraphTeamListByAdminDgraphUID(ctx, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphTeamListByAdminDgraphUID Failed to get dgraph team list info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got team list!", "data": dgraphTeamList})

}

func ArchiveTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputTeamInfo adapter.CreateOrUpdateTeamInput
	err := json.NewDecoder(r.Body).Decode(&inputTeamInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	teamUUID, err := uuid.Parse(inputTeamInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTeam Failed to parse team string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse team string to uuid",
			"err": err,
		})
		return
	}

	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	dgraphTeam, err := business.GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx, inputTeamInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTeam Failed to get team dgraph info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get team dgraph info",
			"err": err,
		})
		return
	}

	// The demo's own team stays for every visitor (business/DemoGuard). The
	// visitor is not an admin, so this is for the day somebody makes it one.
	if demoGuard.KeepsFromVisitor(userInfo.UserPostgresInfo.EmailID, dgraphTeam.CreatedAt) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoSeededMsg})
		return
	}

	err = business.ArchiveTeamByTeamUUID(ctx, teamUUID, dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ArchiveTeam Failed to archive team err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to archive team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Archived team!"})

}

func GetTeamInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	teamUUIDString := chi.URLParam(r, "team_uuid")

	_, err := uuid.Parse(teamUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetTeamInfo Failed to parse team string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse team string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeam, err := business.GetTeamDgraphInfo(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetTeamInfo Failed to get team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get team info",
		})
		return
	}

	if dgraphTeam.IsMember == 0 && !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got teamInfo!", "data": dgraphTeam})

}

func UnArchiveTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputTeamInfo adapter.CreateOrUpdateTeamInput
	err := json.NewDecoder(r.Body).Decode(&inputTeamInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTeam Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	teamUUID, err := uuid.Parse(inputTeamInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTeam Failed to parse team string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse team string to uuid",
			"err": err,
		})
		return
	}

	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	dgraphTeam, err := business.GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx, inputTeamInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTeam Failed to get team dgraph info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get team dgraph info",
			"err": err,
		})
		return
	}

	err = business.UnArchiveTeamByTeamUUID(ctx, teamUUID, dgraphTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnArchiveTeam Failed to archive team err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to archive team",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "UnArchived team!"})

}

func GetAllTeamsList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	pageValue := r.URL.Query()["pageIndex"]
	limitValue := r.URL.Query()["pageSize"]

	pageIndex := 0
	pageSize := 20

	if len(pageValue) > 0 {
		pageInt, err := strconv.Atoi(pageValue[0])
		if err == nil {
			pageIndex = pageInt
		}
	}

	if len(limitValue) > 0 {
		limitInt, err := strconv.Atoi(limitValue[0])
		if err == nil {
			pageSize = limitInt
		}
	}

	dgraphTeams, hasMore, err := business.GetAllTeamDgraphInfo(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllTeamsList Failed to get all teams list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get all teams list",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":      "Sucessful got teams list",
		"data":     dgraphTeams,
		"has_more": hasMore,
	})
}
