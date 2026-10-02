package controllers

// The inbox: a person's own Gmail inside OneCamp. Every handler resolves the
// user from the session, so it only ever reads or sends that person's mail.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// InboxUser resolves the signed-in person; exported for the AI summary handler.
func InboxUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	info, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return uuid.Nil, false
	}
	return info.UserPostgresInfo.Id, true
}

// InboxFail answers a failed mailbox call; exported for the AI summary handler.
// Not connected and reconnect are codes of their own, so the page offers the
// connect button instead of an error.
func InboxFail(w http.ResponseWriter, r *http.Request, err error, what string) {
	var input connectorBusiness.InputError
	if errors.As(err, &input) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": input.Error()})
		return
	}
	if errors.Is(err, connectorBusiness.ErrNotConnected) {
		// The demo's shared visitor can never connect (see NoPersonalAccountsInDemo),
		// so its page explains that instead of offering a button that is refused.
		if info, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok &&
			helpers.IsDemoVisitor(info.UserPostgresInfo.EmailID) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "demo", "msg": helpers.DemoPersonalAccountMsg})
			return
		}
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_connected", "msg": "Connect Gmail to see your inbox here."})
		return
	}
	switch connectorBusiness.ClassifyAPIError(err) {
	case connectorBusiness.ProblemExpired, connectorBusiness.ProblemPermissions:
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "reconnect",
			"msg": "Your Gmail connection has expired or lost access. Reconnect it to see your inbox here."})
	case connectorBusiness.ProblemAPIDisabled:
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{
			"msg": "The Gmail API is not enabled in this workspace's Google Cloud project. An admin needs to enable it."})
	case connectorBusiness.ProblemRateLimited:
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Gmail is busy. Try again in a minute."})
	case connectorBusiness.ProblemNotFound:
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That conversation is no longer in your Gmail."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/inbox %s: %v", what, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Gmail did not answer. Try again in a moment."})
	}
}

// GetInbox handles GET /connectors/gmail/inbox?q=&page_token=.
func GetInbox(w http.ResponseWriter, r *http.Request) {
	uid, ok := InboxUser(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 500 {
		q = q[:500]
	}
	page, err := connectorBusiness.GmailInbox(r.Context(), uid, q, r.URL.Query().Get("page_token"))
	if err != nil {
		InboxFail(w, r, err, "list")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// GetInboxThread handles GET /connectors/gmail/threads/{id}.
func GetInboxThread(w http.ResponseWriter, r *http.Request) {
	uid, ok := InboxUser(w, r)
	if !ok {
		return
	}
	d, err := connectorBusiness.GmailThread(r.Context(), uid, chi.URLParam(r, "id"))
	if err != nil {
		InboxFail(w, r, err, "thread")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}

// ReplyInboxThread handles POST /connectors/gmail/threads/{id}/reply {body}.
// The person wrote the reply and pressed Send: this is their own action.
func ReplyInboxThread(w http.ResponseWriter, r *http.Request) {
	uid, ok := InboxUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid body"})
		return
	}
	id, err := connectorBusiness.GmailReply(r.Context(), uid, chi.URLParam(r, "id"), in.Body)
	if err != nil {
		InboxFail(w, r, err, "reply")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]string{"id": id}})
}
