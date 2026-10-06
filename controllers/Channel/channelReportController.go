package controllers

// Per-channel control of the weekly channel report.
//
// The org switch (ai_settings.team_report_enabled) is the ceiling; a channel
// opts in beneath it. Both must be true for a report to post, so an org admin
// can still stop the whole feature in one place and a channel admin can never
// turn it on against that.
//
// When the org switch is off the FE must not show the control at all, so the
// GET reports the org state alongside the channel's own choice rather than
// leaving the client to guess from a 403.

import (
	"context"
	"encoding/json"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// channelAccess answers both questions this endpoint asks — may the caller SEE
// the channel, and may they CHANGE its settings — from ONE Dgraph read.
//
// canViewChannel and a separate moderator check would each fetch the same
// channel node, so the GET below would query Dgraph twice per request to learn
// two facts that arrive together.
//
// canManage is deliberately stricter than canManageChannelMembership, which
// lets any member of a PUBLIC channel act: this setting causes an automated post
// into the channel every week, so it is a moderator decision in public channels
// too. Fails closed, so an unreadable channel is neither viewable nor managed.
func channelAccess(ctx context.Context, channelUUID uuid.UUID, userDgraphUID string) (canView, canManage bool) {
	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userDgraphUID)
	if err != nil || channelInfo == nil || channelInfo.IsPrivate == nil {
		return false, false
	}
	canManage = channelInfo.IsAdmin != 0
	if *channelInfo.IsPrivate {
		return channelInfo.IsMember != 0, canManage
	}
	return true, canManage
}

// channelWeeklyReportState is the shape both handlers return, so the FE reads
// the same fields whether it just loaded the screen or just toggled the switch.
type channelWeeklyReportState struct {
	// OrgEnabled is the workspace ceiling. When false the FE hides the control
	// entirely; there is nothing a channel admin can do beneath an org "off".
	OrgEnabled bool `json:"org_enabled"`
	// Enabled is this channel's own opt-in. Meaningless while OrgEnabled is false.
	Enabled bool `json:"enabled"`
	// CanManage says whether THIS caller may change it, so the FE can show the
	// state read-only to members instead of hiding it from them.
	CanManage bool `json:"can_manage"`
}

// GetChannelWeeklyReport handles GET /ch/{channelId}/weekly-report
func GetChannelWeeklyReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	canView, canManage := channelAccess(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if !canView {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	state, err := readChannelWeeklyReportState(ctx, channelUUID, canManage)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetChannelWeeklyReport err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load the weekly report setting"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": state})
}

// SetChannelWeeklyReport handles POST /ch/{channelId}/weekly-report
func SetChannelWeeklyReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	_, canManage := channelAccess(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if !canManage {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only channel moderators can change this"})
		return
	}

	// The org ceiling is re-checked on the WRITE, not just reflected in the
	// read. Hiding the control in the FE is a courtesy; this is the rule.
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelWeeklyReport settings err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load workspace settings"})
		return
	}
	if !settings.Enabled || !settings.TeamReportEnabled {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "The weekly channel report is turned off for this workspace"})
		return
	}

	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "enabled is required"})
		return
	}

	if err := aiModels.SetChannelTeamReportEnabled(ctx, channelUUID, *body.Enabled, userInfo.UserPostgresInfo.Id); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelWeeklyReport err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save the weekly report setting"})
		return
	}

	state, err := readChannelWeeklyReportState(ctx, channelUUID, canManage)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelWeeklyReport reload err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Saved, but failed to reload the setting"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": state})
}

// readChannelWeeklyReportState assembles the state both handlers return. canManage
// is passed in rather than re-derived, so the caller's single access read serves
// both the authorization decision and the response.
func readChannelWeeklyReportState(ctx context.Context, channelUUID uuid.UUID, canManage bool) (channelWeeklyReportState, error) {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return channelWeeklyReportState{}, err
	}
	orgEnabled := settings.Enabled && settings.TeamReportEnabled

	enabled := false
	if orgEnabled {
		// Only meaningful beneath an org "on", and skipping the query when the
		// feature is off keeps the common case to a single read.
		if enabled, err = aiModels.ChannelTeamReportEnabled(ctx, channelUUID); err != nil {
			return channelWeeklyReportState{}, err
		}
	}

	return channelWeeklyReportState{
		OrgEnabled: orgEnabled,
		Enabled:    enabled,
		CanManage:  canManage,
	}, nil
}
