// Package controllers (Connector) exposes the per-user connector HTTP API:
// list providers + connection status, start an OAuth connect, handle the
// provider callback, and disconnect. All authed endpoints derive the user from
// the request context, so a user can only ever manage their own connectors.
package controllers

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// connectorStatusView is the per-provider status returned to the FE.
type connectorStatusView struct {
	ID          string                        `json:"id"`
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	IconKey     string                        `json:"icon_key"`
	Permissions []connectorBusiness.ScopeInfo `json:"permissions"`
	Connected   bool                          `json:"connected"`
}

// ListConnectors handles GET /connectors — providers + this user's connection
// status for each.
func ListConnectors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}
	uid := userInfo.UserPostgresInfo.Id

	providers := connectorBusiness.Providers()
	out := make([]connectorStatusView, 0, len(providers))
	for _, p := range providers {
		out = append(out, connectorStatusView{
			ID:          p.ID,
			Name:        p.Name,
			Description: p.Description,
			IconKey:     p.IconKey,
			Permissions: p.Permissions,
			Connected:   connectorBusiness.IsConnected(ctx, uid, p.ID),
		})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"connectors": out}})
}

// StartConnect handles GET /connectors/{provider}/connect — returns the
// provider authorize URL the FE redirects the user to.
func StartConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}
	provider := chi.URLParam(r, "provider")
	// ?return=inbox brings the person back to the page they connected from.
	url, err := connectorBusiness.BuildAuthURL(ctx, userInfo.UserPostgresInfo.Id, provider, r.URL.Query().Get("return"))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Connector/StartConnect err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"url": url}})
}

// Disconnect handles POST /connectors/{provider}/disconnect.
func Disconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}
	provider := chi.URLParam(r, "provider")
	if err := connectorBusiness.Disconnect(ctx, userInfo.UserPostgresInfo.Id, provider); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to disconnect"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Disconnected"})
}

// HandleCallback handles GET /connector/oauth/callback. The state nonce is the
// credential (it binds the flow to the initiating user), so this is mounted
// unauthenticated like the calendar/GitHub callbacks. It redirects back to the
// page the person connected from (the connectors settings page unless they
// named another) with a status query param.
func HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	// A consent screen opened before the demo refused connections must not
	// land a token on the shared visitor either.
	if helpers.DemoMode() {
		http.Redirect(w, r, connectorReturnURL("")+"?connector=error", http.StatusFound)
		return
	}
	res, err := connectorBusiness.HandleCallback(ctx, state, code)
	dest := connectorReturnURL(res.ReturnPage)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Connector/HandleCallback err: %+v", err)
		http.Redirect(w, r, dest+"?connector=error", http.StatusFound)
		return
	}
	http.Redirect(w, r, dest+"?connector=success&provider="+url.QueryEscape(res.ProviderID), http.StatusFound)
}

// connectorReturnURL builds the FE destination for the post-OAuth redirect:
// a page from connectorBusiness.ReturnPages, or the connectors settings page.
func connectorReturnURL(returnPage string) string {
	path, ok := connectorBusiness.ReturnPages[returnPage]
	if !ok {
		path = "/app/settings/connectors"
	}
	feHost := os.Getenv("FE_HOST_DOMAIN")
	if feHost == "" {
		feHost = os.Getenv("FRONTEND_DOMAIN")
	}
	if feHost == "" {
		return "http://localhost:3001" + path
	}
	proto := "https://"
	if strings.Contains(feHost, "localhost") || strings.Contains(feHost, "127.0.0.1") {
		proto = "http://"
	}
	return proto + feHost + path
}
