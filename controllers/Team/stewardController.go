package controllers

import (
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/Team"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// MakeMeTeamAdmin makes the workspace admin asking an admin of a team, and so
// the steward of its projects: who manages their people and archives them
// (Work Access). It is how a workspace admin reaches any project's
// stewardship: one action, written to the audit log before it is taken, and
// never a read that lets them in quietly. POST /admin/teams/{team_uuid}/admins/me.
// For workspace admins.
func MakeMeTeamAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me, ok := userModels.FromContext(ctx)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	if !me.UserPostgresInfo.IsAdmin {
		helpers.WriteForbidden(w, "Only the workspace's admins can do that.")
		return
	}
	teamID, validID := helpers.UUIDParam(w, r, "team_uuid")
	if !validID {
		return
	}
	team, err := business.GetBasicDgraphTeamInfoByUUID(ctx, teamID.String(), me.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "team", "controllers/MakeMeTeamAdmin")
		return
	}
	if team == nil || team.Uuid == "" {
		helpers.WriteNotFound(w, "That team isn't there.")
		return
	}
	if helpers.IsSoftDeleted(team.DeletedAt) {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That team is archived. Restore it first."})
		return
	}
	data := map[string]any{"team_uuid": team.Uuid, "changed": false}
	if team.IsAdmin > 0 {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "You're already one of its admins.", "data": data})
		return
	}
	// No record, no action: a workspace admin taking a team's stewardship is
	// written down before it happens.
	meta := map[string]interface{}{"team_uuid": team.Uuid, "team_name": team.Name}
	if err := auditBusiness.RecordWritten(r, "team.admin.self", auditBusiness.CategorySecurity,
		"Made themselves an admin of a team", meta); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/MakeMeTeamAdmin audit err: %+v", err)
		helpers.WriteRetryLater(w, "That couldn't be recorded in the audit log, so it wasn't done.")
		return
	}
	if err := business.BecomeAdmin(ctx, teamID, me.UserDgraphInfo.Uid, me.UserDgraphInfo.Uuid); err != nil {
		auditBusiness.Record(r, "team.admin.self_failed", auditBusiness.CategorySecurity,
			"Making themselves an admin of a team didn't take", meta)
		helpers.WriteRetryLater(w, "Couldn't make you one of its admins.")
		return
	}
	data["changed"] = true
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "You're now one of its admins.", "data": data})
}
