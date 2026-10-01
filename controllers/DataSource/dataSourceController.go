package controllers

// HTTP handlers for external, read-only data sources.
//
// Route grouping (see router.go) mirrors the permission model:
//   - CONFIG routes (create/update/delete/enable/test) are gated with the
//     agent.manage capability, like MCP servers.
//   - QUERY/browse routes (list-queryable, schema) run at member auth; the
//     business layer enforces per-source visibility (private = creator+admins,
//     workspace = any member) so a member never sees a source they can't query.
//
// The credential is never accepted or returned in cleartext beyond create/
// update input; responses are the safe business.View (HasPassword only).

import (
	"encoding/json"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/DataSource"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func actorFrom(r *http.Request) business.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return business.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
	}
}

// writeErr is the single error surface for these routes. Forbidden/not-found
// keep their existing responses; everything else goes through the business
// layer's sanitizer, which returns a STABLE, admin-useful sentence (unreachable
// host / bad credentials / unknown database / TLS failure / policy refusal /
// validation message) instead of raw driver text. When the failure came from the
// external database, the full wrapped error — driver wording, SQLSTATE, host,
// schema, SQL — is logged server-side only.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have access to this data source"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "data source not found"})
	default:
		msg, external := business.PublicMessage(err)
		if external {
			helpers.LogErrorWithContext(r.Context(), "controllers/DataSource external err: %+v", err)
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": msg})
	}
}

func idParam(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

// List GET /data-sources — management list (agent.manage gated at the router).
func List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.List(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DataSource List err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load data sources"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// Get GET /data-sources/{id} — safe view; query-gated in the business layer.
func Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	v, err := business.Get(ctx, id, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// Create POST /data-sources — agent.manage gated.
func Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in business.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.Create(ctx, in, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "data_source.create", auditBusiness.CategoryIntegration,
		"Connected data source: "+v.Name,
		map[string]interface{}{"data_source_id": v.Id, "engine": v.Engine, "host": v.Host, "database": v.Database, "visibility": v.Visibility})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": v})
}

// Update POST /data-sources/{id}/update — manage-gated in the business layer.
func Update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	var in business.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.Update(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "data_source.update", auditBusiness.CategoryIntegration,
		"Updated data source: "+v.Name,
		map[string]interface{}{"data_source_id": v.Id, "host": v.Host, "database": v.Database, "visibility": v.Visibility})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// SetEnabled POST /data-sources/{id}/enabled — body { "enabled": bool }.
func SetEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetEnabled(ctx, id, body.Enabled, actorFrom(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	action, summary := "data_source.disable", "Disabled data source"
	if body.Enabled {
		action, summary = "data_source.enable", "Enabled data source"
	}
	auditBusiness.Record(r, action, auditBusiness.CategoryIntegration, summary,
		map[string]interface{}{"data_source_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// Delete POST /data-sources/{id}/delete — manage-gated.
func Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	if err := business.Delete(ctx, id, actorFrom(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "data_source.delete", auditBusiness.CategoryIntegration,
		"Removed data source", map[string]interface{}{"data_source_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// TestNewConnection POST /data-sources/test-connection — validate an UNSAVED
// connection (agent.manage gated) so an operator can check credentials before
// saving. Body is the same shape as create; nothing is persisted.
func TestNewConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in business.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.TestConnectionConfig(ctx, in, actorFrom(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "data_source.test", auditBusiness.CategoryIntegration,
		"Tested a data source connection (pre-save)",
		map[string]interface{}{"host": in.Host, "database": in.Database, "engine": in.Engine})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "connection ok"})
}

// TestConnection POST /data-sources/{id}/test — manage-gated read-only ping.
func TestConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	if err := business.TestConnection(ctx, id, actorFrom(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	auditBusiness.Record(r, "data_source.test", auditBusiness.CategoryIntegration,
		"Tested data source connection", map[string]interface{}{"data_source_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "connection ok"})
}

// ListQueryable GET /data-sources/queryable — the enabled sources the caller
// may query (member auth; visibility enforced in business). Powers the FE
// source picker without exposing config the caller can't manage.
func ListQueryable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListQueryable(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DataSource ListQueryable err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load data sources"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// Aggregate POST /data-sources/{id}/aggregate — run a typed, deterministic,
// read-only aggregation. Query-gated (member auth + per-source visibility).
func Aggregate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	var spec business.AggSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, _, err := business.RunAggregate(ctx, id, spec, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// QueryPlan POST /data-sources/{id}/query-plan — run a typed, deterministic,
// read-only MULTI-STEP plan (several metrics, having, share-of-total). Query-
// gated (member auth + per-source visibility).
func QueryPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	var plan business.QueryPlan
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, _, err := business.RunPlan(ctx, id, plan, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// Schema GET /data-sources/{id}/schema — introspect tables/columns.
// Query-gated (member auth + per-source visibility) so a member can browse a
// source they may query; a private source they don't own returns 404/403.
func Schema(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r)
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid data source id"})
		return
	}
	tables, err := business.Introspect(ctx, id, actorFrom(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": tables})
}
