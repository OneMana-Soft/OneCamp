package controllers

// HTTP handlers for Tables. Mounted under the authenticated member group (any
// member can create/own tables; per-table visibility governs access, enforced
// in the business layer). Row writes broadcast over MQTT for live collab.

import (
	"encoding/json"
	"net/http"
	"strconv"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	business "github.com/akashc777/OneCamp/business/DataTable"
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

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have access to this table"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "table not found"})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

func idParam(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

// ListTables GET /tables
func ListTables(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListTables(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListTables err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load tables"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetTable GET /tables/{id} — returns the full bundle (table + fields + views + rows).
func GetTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	bundle, err := business.GetBundle(ctx, id, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": bundle})
}

// CreateTable POST /tables
func CreateTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in business.TableInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	t, err := business.CreateTable(ctx, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": t})
}

// GenerateTable POST /tables/generate — create a table (header + typed columns
// + seed rows) from a natural-language prompt. Body: { "prompt": string }.
func GenerateTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	// Ground the generation in the user's workspace: build the SAME RAG context
	// the AI assistant uses (embedding + k-NN over content this user can access),
	// so "a table of action items from #engineering" can seed real rows. Scoped
	// to the caller, so it never sees content they couldn't. Best-effort: on any
	// failure we fall back to prompt-only generation.
	var workspaceContext string
	if userInfo, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
		if text, _, cerr := aiBusiness.BuildUserContextPublic(ctx, &userInfo, body.Prompt, nil); cerr == nil {
			workspaceContext = text
		}
	}
	t, err := business.GenerateTable(ctx, actorFrom(r), body.Prompt, workspaceContext)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": t})
}

// UpdateTable PUT /tables/{id}
func UpdateTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.TableInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	t, err := business.UpdateTable(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// DeleteTable DELETE /tables/{id}
func DeleteTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	if err := business.DeleteTable(ctx, id, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// ListRows GET /tables/{id}/rows?limit&offset
func ListRows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	rows, err := business.ListRows(ctx, id, actorFrom(r), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rows})
}

// AggregateTable POST /tables/{id}/aggregate — read-only. Computes a grouped
// aggregation (count/sum/avg/min/max, optional group-by + filters) over the
// table's rows the caller may see, powering the table "chart view" and any
// client that wants a totals/breakdown/trend without pulling every row. The
// heavy lifting (validation, folding, bounding) is the shared, pure engine.
func AggregateTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var spec business.QuerySpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, _, err := business.AggregateTable(ctx, id, actorFrom(r), spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// QueryPlan POST /tables/{id}/query-plan — read-only. Runs the deterministic,
// inspectable multi-step query plan (filter → group → one-or-more metrics →
// having → share-of-total → sort → limit) over the rows the caller may see,
// and returns the PlanResult. This is what powers the "editable + re-runnable"
// Query plan card: the agent emits the plan it ran, the reader can tweak the
// bounded knobs (filter values, having thresholds, limit, order) and re-run it
// live, hitting the SAME pure engine the agent used — so a human can steer the
// data question and get an identical-methodology answer, not a fresh guess. The
// permission model is re-checked per call inside the business layer.
func QueryPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var plan business.QueryPlan
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, _, err := business.ExecutePlan(ctx, id, actorFrom(r), plan)
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// CreateRow POST /tables/{id}/rows
func CreateRow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.RowInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, err := business.CreateRow(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": row})
}

// UpdateRow PUT /tables/{id}/rows/{rowId}
func UpdateRow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	rowId, ok2 := idParam(r, "rowId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in business.RowInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, err := business.UpdateRow(ctx, id, rowId, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": row})
}

// DeleteRow DELETE /tables/{id}/rows/{rowId}
func DeleteRow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	rowId, ok2 := idParam(r, "rowId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	if err := business.DeleteRow(ctx, id, rowId, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// FillAIColumn POST /tables/{id}/fields/{fieldId}/ai-fill — evaluate an AI
// column's prompt over each row (or the given subset) and write the cells.
// Body: { "row_ids"?: ["..."] }.
func FillAIColumn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var body struct {
		RowIds []string `json:"row_ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body optional (no rows => all)

	var rowIds []uuid.UUID
	for _, s := range body.RowIds {
		if rid, perr := uuid.Parse(s); perr == nil {
			rowIds = append(rowIds, rid)
		}
	}

	res, err := business.FillAIColumn(ctx, id, fieldId, rowIds, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// CreateField POST /tables/{id}/fields
func CreateField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.FieldInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	f, err := business.CreateField(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": f})
}

// UpdateField PUT /tables/{id}/fields/{fieldId}
func UpdateField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in business.FieldInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.UpdateField(ctx, id, fieldId, in, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// DeleteField DELETE /tables/{id}/fields/{fieldId}
func DeleteField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	if err := business.DeleteField(ctx, id, fieldId, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// CreateView POST /tables/{id}/views
func CreateView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.ViewInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.CreateView(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": v})
}

// UpdateView PUT /tables/{id}/views/{viewId}
func UpdateView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	viewId, ok2 := idParam(r, "viewId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in business.ViewInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.UpdateView(ctx, id, viewId, in, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// DeleteView DELETE /tables/{id}/views/{viewId}
func DeleteView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(r, "id")
	viewId, ok2 := idParam(r, "viewId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	if err := business.DeleteView(ctx, id, viewId, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}
