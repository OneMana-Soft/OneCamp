package controllers

// HTTP handlers for managing the current user's API tokens. Mounted under the
// normal authenticated member group (a token acts as you, so you manage your
// own). The plaintext secret is returned ONLY in the create response.

import (
	"encoding/json"
	"net/http"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/ApiToken"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func currentUserID(r *http.Request) uuid.UUID {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return userInfo.UserPostgresInfo.Id
}

// currentUserIsAdmin reports the caller's workspace-admin flag, read from the same
// authenticated UserInfo rather than from the request body — the binding rule below
// depends on it, and a client-supplied flag would decide its own authorization.
func currentUserIsAdmin(r *http.Request) bool {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return userInfo.UserPostgresInfo.IsAdmin
}

// ListScopes GET /api-tokens/scopes — the grantable scope catalog (for the UI).
func ListScopes(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.AllScopes})
}

// ListTokens GET /api-tokens
func ListTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListTokens(ctx, currentUserID(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListTokens err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load tokens"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// CreateToken POST /api-tokens — returns the plaintext once.
func CreateToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Name          string   `json:"name"`
		Scopes        []string `json:"scopes"`
		ExpiresInDays int      `json:"expires_in_days"`
		// AgentID optionally binds this credential to an agent identity. Omitted or
		// blank means a plain integration credential, which is what every token was
		// before this field existed.
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	var agentID *uuid.UUID
	if trimmed := strings.TrimSpace(body.AgentID); trimmed != "" {
		parsed, perr := uuid.Parse(trimmed)
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
			return
		}
		agentID = &parsed
	}

	created, err := business.CreateToken(ctx, body.Name, body.Scopes, body.ExpiresInDays,
		currentUserID(r), agentID, currentUserIsAdmin(r))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	// Record the agent binding, because "a credential was minted that acts as this
	// agent identity" is a materially different security event from minting a plain
	// one, and the difference has to be visible without joining tables later.
	meta := map[string]interface{}{"token_id": created.Token.Id.String(), "scopes": body.Scopes}
	summary := "Created API token: " + created.Token.Name
	if agentID != nil {
		meta["agent_id"] = agentID.String()
		summary += " (bound to an agent identity)"
	}
	auditBusiness.Record(r, "api_token.create", auditBusiness.CategorySecurity, summary, meta)
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": created})
}

// RevokeToken POST /api-tokens/{id}/revoke
func RevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid token id"})
		return
	}
	if err := business.RevokeToken(ctx, id, currentUserID(r)); err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "token not found"})
		return
	}
	auditBusiness.Record(r, "api_token.revoke", auditBusiness.CategorySecurity,
		"Revoked API token", map[string]interface{}{"token_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "revoked"})
}
