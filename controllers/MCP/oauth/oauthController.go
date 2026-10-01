// Package oauth serves the OAuth 2.1 endpoints that let an
// MCP client sign a person in. See business/MCPServer/oauth for the why.
package oauth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/MCPServer/oauth"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxBody = 64 << 10

// writeOAuth answers in the shape OAuth clients parse: the bare object, never
// wrapped, and never cached.
func writeOAuth(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthErr(w http.ResponseWriter, e *business.Error) {
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	writeOAuth(w, status, e)
}

// ProtectedResource GET /.well-known/oauth-protected-resource[/v1/mcp]
func ProtectedResource(w http.ResponseWriter, _ *http.Request) {
	writeOAuth(w, http.StatusOK, business.ProtectedResourceMetadata())
}

// AuthorizationServer GET /.well-known/oauth-authorization-server
func AuthorizationServer(w http.ResponseWriter, _ *http.Request) {
	writeOAuth(w, http.StatusOK, business.AuthorizationServerMetadata())
}

// Register POST /oauth/register
func Register(w http.ResponseWriter, r *http.Request) {
	var in business.RegisterInput
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeOAuthErr(w, &business.Error{Code: "invalid_client_metadata", Description: "the body must be a JSON client registration"})
		return
	}
	out, e := business.Register(r.Context(), in)
	if e != nil {
		writeOAuthErr(w, e)
		return
	}
	writeOAuth(w, http.StatusCreated, out)
}

// Authorize GET /oauth/authorize
func Authorize(w http.ResponseWriter, r *http.Request) {
	res := business.Authorize(r.Context(), r.URL.Query())
	http.Redirect(w, r, res.Redirect, http.StatusFound)
}

// Token POST /oauth/token
func Token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		writeOAuthErr(w, &business.Error{Code: "invalid_request", Description: "send application/x-www-form-urlencoded"})
		return
	}
	out, e := business.Token(r.Context(), r.PostForm)
	if e != nil {
		writeOAuthErr(w, e)
		return
	}
	writeOAuth(w, http.StatusOK, out)
}

// Revoke POST /oauth/revoke
func Revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err == nil {
		business.Revoke(r.Context(), r.PostForm.Get("token"))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// The consent page's API. Session-authenticated: the person approving is the
// one signed in to this workspace.

func actorFrom(r *http.Request) agentBusiness.Actor {
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return agentBusiness.Actor{UserID: u.UserPostgresInfo.Id, IsAdmin: u.UserPostgresInfo.IsAdmin, DgraphUID: u.UserDgraphInfo.Uid}
}

func requestID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid sign-in request"})
		return uuid.Nil, false
	}
	return id, true
}

func writeConsentErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, business.ErrRequestGone):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": err.Error(), "code": "gone"})
	case errors.Is(err, business.ErrSurfaceClosed):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": err.Error(), "code": "surface_closed"})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/OAuth consent err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// GetConsent GET /oauth/requests/{id}
func GetConsent(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	view, err := business.Consent(r.Context(), id, actorFrom(r))
	if err != nil {
		writeConsentErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// ApproveConsent POST /oauth/requests/{id}/approve
func ApproveConsent(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	var in business.ApproveInput
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, err := business.Approve(r.Context(), id, actorFrom(r), in)
	if err != nil {
		writeConsentErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "agent.connect", auditBusiness.CategorySecurity,
		"Connected an outside agent as "+res.AgentName, map[string]interface{}{
			"agent_id":      res.AgentID.String(),
			"agent_created": res.Created,
			"scopes":        in.Scopes,
		})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// DenyConsent POST /oauth/requests/{id}/deny
func DenyConsent(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	redirect, err := business.Deny(r.Context(), id)
	if err != nil {
		writeConsentErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"redirect": redirect}})
}

// ListMyConnections handles GET /me/assistants: the outside agents the caller
// has connected, and whether the workspace lets them in at all.
func ListMyConnections(w http.ResponseWriter, r *http.Request) {
	view, err := business.MyConnections(r.Context(), actorFrom(r).UserID)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/MCP/oauth/ListMyConnections failed err: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not load your connected assistants"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// DisconnectMine handles POST /me/assistants/{id}/disconnect: the caller ends
// one of their own connections, and it stops working at once.
func DisconnectMine(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid connection"})
		return
	}
	if err := business.Disconnect(r.Context(), actorFrom(r).UserID, id); err != nil {
		if errors.Is(err, business.ErrNotYourConnection) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/MCP/oauth/DisconnectMine failed err: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not disconnect; try again"})
		return
	}
	auditBusiness.Record(r, "agent.disconnect", auditBusiness.CategorySecurity,
		"Disconnected an outside agent", map[string]interface{}{"connection_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Disconnected"})
}
