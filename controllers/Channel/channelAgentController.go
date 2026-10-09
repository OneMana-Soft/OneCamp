package controllers

// In-channel "AI teammates" management: list the workspace's mention-trigger
// agents and toggle whether each responds in THIS channel (the Slack/Claude-Tag
// "add the AI to a channel" model). Authorization mirrors AddChannelMember: the
// caller must be a member of a public channel, or an admin of a private one;
// and moving an agent is also its owner's (or a workspace admin's), with a
// channel's admins able to take one out of their channel (business/AIAgent
// mayPlaceInChannel). The agent presence itself is expressed through the agent's channel scope
// (enforced by the mention trigger), so this is the in-channel front door to
// the same governance the Agent Builder edits.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	business "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// canManageChannelMembership reports whether the caller may add/remove members
// (and AI teammates) on a channel: a member of a public channel, or an admin of
// a private one. Mirrors the gate in AddChannelMember.
func canManageChannelMembership(ctx context.Context, channelUUID uuid.UUID, userDgraphUID string) bool {
	_, ok := channelManagedBy(ctx, channelUUID, userDgraphUID)
	return ok
}

// channelManagedBy reads the channel as the caller sees it, and reports
// whether they may manage its members (canManageChannelMembership's rule).
func channelManagedBy(ctx context.Context, channelUUID uuid.UUID, userDgraphUID string) (*dgraphStruct.DgraphChannel, bool) {
	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userDgraphUID)
	if err != nil || channelInfo == nil || channelInfo.IsPrivate == nil {
		return nil, false
	}
	if *channelInfo.IsPrivate {
		return channelInfo, channelInfo.IsAdmin != 0
	}
	return channelInfo, channelInfo.IsMember != 0
}

// canViewChannel reports whether the caller may see/compose in a channel:
// a member of a private channel, or any user for a public one. Used to gate
// the channel mention-agents list (who can @mention there).
func canViewChannel(ctx context.Context, channelUUID uuid.UUID, userDgraphUID string) bool {
	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userDgraphUID)
	if err != nil || channelInfo == nil || channelInfo.IsPrivate == nil {
		return false
	}
	if *channelInfo.IsPrivate {
		return channelInfo.IsMember != 0
	}
	return true
}

// ListChannelMentionAgents handles GET /ch/{channelId}/mention-agents
// Returns the AI agents scoped to this channel as @mention typeahead entries,
// so the channel composer can suggest agents only where they have been added.
func ListChannelMentionAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	if !canViewChannel(ctx, channelUUID, userInfo.UserDgraphInfo.Uid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	agents, err := agentBusiness.ListChannelMentionAgents(ctx, channelUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListChannelMentionAgents err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load mention agents"})
		return
	}
	if agents == nil {
		agents = []agentBusiness.ChannelMentionAgent{}
	}
	// Shape matches the user @mention typeahead entries so the FE merges them
	// into the same list.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"users": agents})
}

// ListChannelAITeammates handles GET /ch/{channelId}/ai-teammates
// Returns the active mention agents and whether each is active in this channel.
func ListChannelAITeammates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	if !canManageChannelMembership(ctx, channelUUID, userInfo.UserDgraphInfo.Uid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	options, err := agentBusiness.ListChannelAgentOptions(ctx, channelUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListChannelAITeammates err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load AI teammates"})
		return
	}
	if options == nil {
		options = []agentBusiness.ChannelAgentOption{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": options})
}

type setChannelAITeammateReq struct {
	ChannelID string `json:"channel_id"`
	AgentID   string `json:"agent_id"`
	Enabled   bool   `json:"enabled"`
}

// SetChannelAITeammate handles POST /ch/ai-teammates
// Adds (enabled=true) or removes (enabled=false) an agent from a channel.
func SetChannelAITeammate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req setChannelAITeammateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	channelUUID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	agentUUID, err := uuid.Parse(req.AgentID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	channelInfo, ok := channelManagedBy(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if !ok {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	// Who may move this agent, not only this channel's members: its owner or
	// a workspace admin, and to take it out, also this channel's admins.
	actor := agentBusiness.Actor{UserID: userInfo.UserPostgresInfo.Id, IsAdmin: userInfo.UserPostgresInfo.IsAdmin}
	if err := agentBusiness.SetAgentChannelMembership(ctx, actor, channelInfo.IsAdmin != 0, agentUUID, channelUUID.String(), req.Enabled); err != nil {
		if agentBusiness.IsNotFound(err) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Agent not found"})
			return
		}
		if agentBusiness.IsOnlyChannel(err) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This is the only channel the agent is in, and an agent in no channel answers everywhere, so only its owner or a workspace admin can take it out."})
			return
		}
		if agentBusiness.IsForbidden(err) {
			msg := "Only the agent's owner or a workspace admin can add it to a channel."
			if !req.Enabled {
				msg = "Only the agent's owner, a workspace admin or one of this channel's admins can take it out."
			}
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": msg})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelAITeammate err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update AI teammate"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": map[bool]string{true: "Added to channel", false: "Removed from channel"}[req.Enabled],
	})
}

// GetChannelAIBudget handles GET /ch/{channelId}/ai-budget
// Returns the channel's per-day AI token cap (0 = no cap) and today's spend, so
// the channel members dialog can show "AI today: used / cap". Same management
// gate as the AI-teammates controls.
func GetChannelAIBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	if !canManageChannelMembership(ctx, channelUUID, userInfo.UserDgraphInfo.Uid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	cap, err := business.GetChannelAITokenCap(ctx, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetChannelAIBudget err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load AI budget"})
		return
	}
	modelID := ""
	if mid, mErr := business.GetChannelAIModel(ctx, channelUUID); mErr == nil && mid != nil {
		modelID = mid.String()
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"max_daily_tokens": cap,
		"tokens_today":     ai.ChannelTokenUsageToday(ctx, channelUUID.String()),
		"ai_model_id":      modelID,
	}})
}

type setChannelAIBudgetReq struct {
	ChannelID      string `json:"channel_id"`
	MaxDailyTokens int    `json:"max_daily_tokens"`
	// AIModelID pins the channel's default AI model: nil = leave unchanged,
	// "" = clear the override, a uuid = set it. A pointer so "omitted" and
	// "clear" are distinguishable.
	AIModelID *string `json:"ai_model_id,omitempty"`
}

// SetChannelAIBudget handles POST /ch/ai-budget
// Sets the channel's per-day AI token cap (0 = no cap). Same management gate.
func SetChannelAIBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req setChannelAIBudgetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	channelUUID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	if !canManageChannelMembership(ctx, channelUUID, userInfo.UserDgraphInfo.Uid) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	if req.MaxDailyTokens < 0 {
		req.MaxDailyTokens = 0
	}
	if err := business.SetChannelAITokenCap(ctx, channelUUID, req.MaxDailyTokens); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelAIBudget err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update AI budget"})
		return
	}
	// Per-channel default model, when the request carries the field. Empty
	// string clears the override; a value is validated against the allowlist by
	// the business layer (a bad id is a 400, not a silent no-op).
	if req.AIModelID != nil {
		var modelID *uuid.UUID
		if trimmed := strings.TrimSpace(*req.AIModelID); trimmed != "" {
			parsed, perr := uuid.Parse(trimmed)
			if perr != nil {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
				return
			}
			modelID = &parsed
		}
		if err := business.SetChannelAIModel(ctx, channelUUID, modelID); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
			return
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated channel AI budget"})
}
