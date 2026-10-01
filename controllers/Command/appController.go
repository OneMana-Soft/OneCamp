package controllers

// Admin app-platform HTTP handlers. Mounted under the admin router (gated by
// VerifyAdminAuthOnlyPostgres), so every handler assumes a verified admin.
// Secrets are never returned — see adapter.AppView (has_* booleans only).

import (
	"encoding/json"
	"net/http"
	"os"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	business "github.com/akashc777/OneCamp/business/Command"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ListApps handles GET /admin/apps
func ListApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	apps, err := business.ListApps(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/ListApps err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load apps"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"apps": apps}})
}

// GetApp handles GET /admin/apps/{appId}
func GetApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	app, err := business.GetApp(ctx, id)
	if err != nil || app == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "App not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": app})
}

// CreateApp handles POST /admin/apps
func CreateApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req commandAdapter.CreateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	app, err := business.CreateApp(ctx, req, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/CreateApp err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	auditBusiness.Record(r, "app.install", auditBusiness.CategoryApp,
		"Installed app: "+app.Name, map[string]interface{}{"slug": app.Slug, "kind": app.Kind})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": app})
}

// UpdateApp handles PATCH /admin/apps/{appId}
func UpdateApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	var req commandAdapter.UpdateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	app, err := business.UpdateApp(ctx, id, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/UpdateApp err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": app})
}

// SetAppEnabled handles POST /admin/apps/{appId}/enabled  body: {enabled:bool}
func SetAppEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	if err := business.SetAppEnabled(ctx, id, body.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update app"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated"})
}

// DeleteApp handles DELETE /admin/apps/{appId}
func DeleteApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	if err := business.DeleteApp(ctx, id); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to delete app"})
		return
	}
	auditBusiness.Record(r, "app.uninstall", auditBusiness.CategoryApp,
		"Removed app", map[string]interface{}{"app_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// GetOAuthInstallURL handles GET /admin/apps/{appId}/oauth-url
func GetOAuthInstallURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	redirectURI := appOAuthRedirectURI()
	url, err := business.BuildOAuthInstallURL(ctx, id, redirectURI)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": commandAdapter.OAuthInstallURLResponse{URL: url}})
}

// DisconnectOAuthApp handles POST /admin/apps/{appId}/oauth-disconnect
func DisconnectOAuthApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	if err := business.DisconnectOAuthApp(ctx, id); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to disconnect"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Disconnected"})
}

// TestApp handles POST /admin/apps/{appId}/test — runs a real credential /
// connectivity check so the admin can verify setup before relying on the app.
func TestApp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid app id"})
		return
	}
	result, err := business.TestApp(ctx, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/TestApp err: %+v", err)
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "App not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
}

// HandleOAuthCallback handles GET /admin/apps/oauth/callback (public-ish: the
// state nonce is the credential; the handler validates it). Redirects back to
// the admin apps page with a status query param.
func HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	feHost := os.Getenv("FE_HOST_DOMAIN")
	dest := "/app/admin?tab=apps"
	if feHost != "" {
		dest = "https://" + feHost + "/app/admin?tab=apps"
	}

	appID, err := business.HandleOAuthCallback(ctx, state, code, appOAuthRedirectURI())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/HandleOAuthCallback err: %+v", err)
		http.Redirect(w, r, dest+"&oauth=error", http.StatusFound)
		return
	}
	http.Redirect(w, r, dest+"&oauth=success&app="+appID.String(), http.StatusFound)
}

// appOAuthRedirectURI builds the callback URL the provider redirects to.
// Points at the BE directly (mirrors the GitHub integration) to avoid
// cross-domain cookie issues during the OAuth handshake.
func appOAuthRedirectURI() string {
	host := os.Getenv("BACKEND_DOMAIN")
	if host == "" {
		return "http://localhost:3000/admin/apps/oauth/callback"
	}
	return "https://" + host + "/admin/apps/oauth/callback"
}

// ListMarketplace handles GET /admin/marketplace — the curated app directory
// enriched with this workspace's install state.
func ListMarketplace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListMarketplace(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/ListMarketplace err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load marketplace"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"apps": items}})
}

// InstallTemplate handles POST /admin/marketplace/install  body: {slug}
func InstallTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req commandAdapter.InstallTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	app, err := business.InstallTemplate(ctx, req.Slug, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/InstallTemplate err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	auditBusiness.Record(r, "app.install", auditBusiness.CategoryApp,
		"One-click installed app: "+app.Name, map[string]interface{}{"slug": app.Slug, "source": "marketplace"})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": app})
}

// UninstallTemplate handles POST /admin/marketplace/uninstall  body: {slug}
func UninstallTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req commandAdapter.InstallTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	if err := business.UninstallTemplate(ctx, req.Slug); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/UninstallTemplate err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to uninstall"})
		return
	}
	auditBusiness.Record(r, "app.uninstall", auditBusiness.CategoryApp,
		"One-click uninstalled app", map[string]interface{}{"slug": req.Slug, "source": "marketplace"})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Uninstalled"})
}

// ServeAppIcon handles GET /public/app-icon/{obj_uuid}. App icons are stored as
// public attachments (uploaded through the AV-scanned upload pipeline). MinIO
// presigned URLs expire after a few minutes, which is useless for an icon that
// must render persistently in the admin list and the live command menu. So the
// stored icon_url points at this stable endpoint, which 302-redirects to a
// freshly-minted presigned URL on every request. The endpoint is public (icons
// are non-sensitive branding assets) and only ever resolves PUBLIC-scoped
// attachments, so it can't be used to fish private files.
func ServeAppIcon(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	objUUID := chi.URLParam(r, "obj_uuid")
	if _, err := uuid.Parse(objUUID); err != nil {
		http.Error(w, "invalid icon id", http.StatusBadRequest)
		return
	}

	att, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objUUID, postgresStruct.ATTACHMENT_SRC_PUBLIC)
	if err != nil || att == nil || att.SrcKey != postgresStruct.ATTACHMENT_SRC_PUBLIC {
		http.Error(w, "icon not found", http.StatusNotFound)
		return
	}

	url, err := userBusiness.GetFileURLByObjectName(ctx, att.ObjKey)
	if err != nil || url == "" {
		http.Error(w, "icon unavailable", http.StatusNotFound)
		return
	}

	// Let browsers cache the redirect briefly so a busy command menu doesn't
	// re-presign on every keystroke, while staying well under the presign TTL.
	w.Header().Set("Cache-Control", "private, max-age=120")
	http.Redirect(w, r, url, http.StatusFound)
}
