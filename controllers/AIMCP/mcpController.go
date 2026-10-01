package controllers

// HTTP handlers for managing MCP (Model Context Protocol) servers. Mounted
// under the agent.manage capability group (MCP tools feed the Agent Builder).
// Every mutation is audit-logged. Auth secrets are never returned to clients
// (the model omits them; only a has_auth_secret flag is exposed).
//
// Every tool this package returns — from a live connection test or derived from
// a stored server's cache — is enriched with OneCamp's OWN enforced risk
// classification (read_only / destructive) via business.ClassifyTools, so an
// admin picking tools can see which auto-run, which need approval, and which are
// destructive. The external server's self-reported annotations are never
// returned: they are untrusted and only feed the host-side classifier.

import (
	"encoding/json"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AIMCP"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func writeErr(w http.ResponseWriter, err error) {
	if business.IsNotFound(err) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "MCP server not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
}

// ListServers GET /mcp/servers
func ListServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListServers(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListServers err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load MCP servers"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.NewServerViews(items)})
}

// GetCatalog GET /mcp/catalog — the curated connector catalog, each entry
// flagged installed when a matching server already exists. Read-only; lets the
// admin install a vetted connector in a couple of clicks (the FE prefills the
// add-server dialog from an entry) instead of hand-entering everything.
func GetCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.Catalog(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetCatalog err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load connector catalog"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetServer GET /mcp/servers/{id}
func GetServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid server id"})
		return
	}
	s, err := business.GetServer(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.NewServerView(s)})
}

// CreateServer POST /mcp/servers
func CreateServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in business.ServerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	s, err := business.CreateServer(ctx, in, userInfo.UserPostgresInfo.Id)
	if err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "mcp.create", auditBusiness.CategorySettings,
		"Registered MCP server: "+s.Name, map[string]interface{}{"server_id": s.Id.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": business.NewServerView(s)})
}

// UpdateServer PUT /mcp/servers/{id}. The body's update_secret flag controls
// whether the stored secret is replaced (so the UI can omit it on edit).
func UpdateServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid server id"})
		return
	}
	var body struct {
		business.ServerInput
		UpdateSecret bool `json:"update_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	s, err := business.UpdateServer(ctx, id, body.ServerInput, body.UpdateSecret)
	if err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "mcp.update", auditBusiness.CategorySettings,
		"Updated MCP server: "+s.Name, map[string]interface{}{"server_id": s.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.NewServerView(s)})
}

// SetEnabled POST /mcp/servers/{id}/enabled
func SetEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid server id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetEnabled(ctx, id, body.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "mcp.toggle", auditBusiness.CategorySettings,
		"Toggled MCP server", map[string]interface{}{"server_id": id.String(), "enabled": body.Enabled})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// DeleteServer DELETE /mcp/servers/{id}
func DeleteServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid server id"})
		return
	}
	if err := business.DeleteServer(ctx, id); err != nil {
		writeErr(w, err)
		return
	}
	auditBusiness.Record(r, "mcp.delete", auditBusiness.CategorySettings,
		"Deleted MCP server", map[string]interface{}{"server_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// TestConnection POST /mcp/servers/{id}/test — introspect live and return the
// tools, each labelled with the risk OneCamp enforces for it.
func TestConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid server id"})
		return
	}
	tools, err := business.TestConnection(ctx, id)
	if err != nil {
		if business.IsNotFound(err) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "MCP server not found"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": false, "msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true, "data": business.ClassifyTools(tools)})
}
