package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	adminAudit "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/SlackBridge"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleEvents is Slack's Events API delivery (POST /slack/events). Public:
// the request signature is the credential, checked in the business layer.
func HandleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	res := business.HandleEvents(r.Context(), body,
		r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"))
	if res.ContentType != "" {
		w.Header().Set("Content-Type", res.ContentType)
	}
	w.WriteHeader(res.Status)
	if res.Body != "" {
		_, _ = w.Write([]byte(res.Body))
	}
}

func actor(r *http.Request) *uuid.UUID {
	if id, _, ok := adminAudit.ActorFromContext(r.Context()); ok {
		return &id
	}
	return nil
}

// fail answers an admin request: a mistake the admin can fix is a 400 with
// the reason, anything else a logged 500.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var ue *business.UserError
	switch {
	case errors.As(err, &ue),
		errors.Is(err, business.ErrNotConnected), errors.Is(err, business.ErrBadToken),
		errors.Is(err, business.ErrBadSecret), errors.Is(err, business.ErrChannelMissing):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"status": "failed", "msg": err.Error()})
	case errors.Is(err, business.ErrLinkTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"status": "failed", "msg": err.Error()})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/SlackBridge: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"status": "failed", "msg": "Something went wrong talking to Slack. Try again."})
	}
}

// GetStatus handles GET /admin/slack-bridge.
func GetStatus(w http.ResponseWriter, r *http.Request) {
	st, err := business.GetStatus(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": st})
}

// Connect handles PUT /admin/slack-bridge {bot_token, signing_secret}.
func Connect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BotToken      string `json:"bot_token"`
		SigningSecret string `json:"signing_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"status": "failed", "msg": "invalid body"})
		return
	}
	st, err := business.Connect(r.Context(), in.BotToken, in.SigningSecret, actor(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	adminAudit.Record(r, "slack_bridge.connected", adminAudit.CategoryIntegration,
		"Connected the Slack workspace "+st.TeamName, map[string]interface{}{"team_id": st.TeamID})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": st})
}

// Disconnect handles DELETE /admin/slack-bridge.
func Disconnect(w http.ResponseWriter, r *http.Request) {
	if err := business.Disconnect(r.Context()); err != nil {
		fail(w, r, err)
		return
	}
	adminAudit.Record(r, "slack_bridge.disconnected", adminAudit.CategoryIntegration, "Disconnected Slack", nil)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})
}

// ListSlackChannels handles GET /admin/slack-bridge/slack-channels.
func ListSlackChannels(w http.ResponseWriter, r *http.Request) {
	chans, err := business.SlackChannels(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": chans})
}

// CreateLink handles POST /admin/slack-bridge/links {slack_channel_id, channel_uuid}.
func CreateLink(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SlackChannelID string `json:"slack_channel_id"`
		ChannelUUID    string `json:"channel_uuid"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil || in.SlackChannelID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"status": "failed", "msg": "choose a Slack channel and a OneCamp channel"})
		return
	}
	channelUUID, err := uuid.Parse(in.ChannelUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"status": "failed", "msg": "choose a OneCamp channel"})
		return
	}
	link, err := business.Link(r.Context(), in.SlackChannelID, channelUUID, actor(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	adminAudit.Record(r, "slack_bridge.linked", adminAudit.CategoryIntegration,
		"Linked Slack #"+link.SlackChannelName+" with #"+link.ChannelName,
		map[string]interface{}{"slack_channel_id": link.SlackChannelID, "channel_uuid": link.ChannelUUID})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": link})
}

// DeleteLink handles DELETE /admin/slack-bridge/links/{id}.
func DeleteLink(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"status": "failed", "msg": "invalid link id"})
		return
	}
	ok, err := business.Unlink(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"status": "failed", "msg": "link not found"})
		return
	}
	adminAudit.Record(r, "slack_bridge.unlinked", adminAudit.CategoryIntegration, "Unlinked a Slack channel",
		map[string]interface{}{"link_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})
}
