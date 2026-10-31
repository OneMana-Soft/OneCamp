package controllers

// HTTP handlers for Tables. Mounted under the authenticated member group (any
// member can create/own tables; per-table visibility governs access, enforced
// in the business layer). Row writes broadcast over MQTT for live collab.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
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

// readerCtx is the request's context with the reader's time zone (?tz=, an
// IANA name), where a formula's TODAY() is.
func readerCtx(r *http.Request) context.Context {
	return business.WithZone(r.Context(), r.URL.Query().Get("tz"))
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you don't have access to this table"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "table not found"})
	case business.IsRowNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "row not found"})
	case business.IsViewNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "view not found"})
	case business.IsUnavailable(err):
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	case business.IsViewsUnavailable(err):
		helpers.WriteRetryLater(w, "Couldn't reach the table's views.")
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
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	// The member's grid marks the rows people are talking about.
	business.WithCommentCounts(ctx, bundle.Rows)
	business.WithPages(ctx, bundle.Rows)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": bundle})
}

// CreateTable POST /tables
func CreateTable(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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

// UpdateTable POST /tables/{id}/update {name?, description?, icon?,
// visibility?}: changes the fields it names, and no others. A change of who
// can see the table is recorded in the audit log.
func UpdateTable(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.TablePatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	t, was, err := business.UpdateTable(ctx, id, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	if t.Visibility != was {
		adminAuditBusiness.Record(r, "table.visibility", adminAuditBusiness.CategorySecurity,
			fmt.Sprintf("Changed who can see table %q from %s to %s", t.Name, was, t.Visibility),
			map[string]interface{}{"table_id": t.Id.String(), "from": was, "to": t.Visibility})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// DeleteTable DELETE /tables/{id}
func DeleteTable(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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

// ListRows GET /tables/{id}/rows?limit&offset, or ?ids= (comma-separated,
// at most 500) for the rows a live message named, in that order, without
// those deleted since.
func ListRows(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var rows []*model.Row
	var err error
	if q := r.URL.Query(); q.Has("ids") {
		ids, perr := business.ParseRowIDs(q.Get("ids"))
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": perr.Error()})
			return
		}
		rows, err = business.RowsByID(ctx, id, actorFrom(r), ids)
	} else {
		limit := helpers.LimitParam(q, 500, 1000)
		offset, _ := strconv.Atoi(q.Get("offset"))
		rows, err = business.ListRows(ctx, id, actorFrom(r), limit, offset)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	business.WithCommentCounts(ctx, rows)
	business.WithPages(ctx, rows)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rows})
}

// SetPresence POST /tables/{id}/presence {row_id?, field_id?, editing?, gone?}
// says where the caller is on a table (gone: that they left) and answers who
// is on it now (tables A7). An open table sends it every 20 s and when its
// selection changes.
func SetPresence(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in struct {
		Row     string `json:"row_id"`
		Field   string `json:"field_id"`
		Editing bool   `json:"editing"`
		Gone    bool   `json:"gone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	people, err := business.SetPresence(ctx, id, actorFrom(r), business.Here{Row: in.Row, Field: in.Field, Editing: in.Editing}, in.Gone, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": people})
}

// AggregateTable POST /tables/{id}/aggregate — read-only. Computes a grouped
// aggregation (count/sum/avg/min/max, optional group-by + filters) over the
// table's rows the caller may see, powering the table "chart view" and any
// client that wants a totals/breakdown/trend without pulling every row. The
// heavy lifting (validation, folding, bounding) is the shared, pure engine.
func AggregateTable(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.RowAsRead(ctx, id, actorFrom(r), row)})
}

// PatchRow POST /tables/{id}/rows/{rowId}/patch {values: {fieldId: value|null}}
// sets the cells it names and leaves the rest of the row as it is now.
func PatchRow(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	rowId, ok2 := idParam(r, "rowId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in struct {
		Values map[string]interface{} `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, err := business.PatchRow(ctx, id, rowId, in.Values, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	// As a read answers it, so the grid puts it back with no fetch.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.RowAsRead(ctx, id, actorFrom(r), row)})
}

// DeleteRow DELETE /tables/{id}/rows/{rowId}
func DeleteRow(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var body struct {
		RowIds  []string `json:"row_ids"`
		Confirm *bool    `json:"confirm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body optional (no rows => all)

	in := business.FillInput{Confirm: body.Confirm}
	if body.Confirm == nil {
		// A web from before fills took the whole column: its fill takes
		// what one did then.
		in.Most = business.LegacyAIFillCells
	}
	for _, s := range body.RowIds {
		if rid, perr := uuid.Parse(s); perr == nil {
			in.RowIds = append(in.RowIds, rid)
		}
	}

	res, err := business.FillAIColumnWith(ctx, id, fieldId, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// StopAIFill POST /tables/{id}/fields/{fieldId}/ai-fill/stop — stop the column's
// running fill (its person, or one who manages the table). Answers the fill
// as it now is, or null when none ran.
func StopAIFill(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	fill, err := business.StopAIFill(ctx, id, fieldId, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": fill})
}

// ListAIFills GET /tables/{id}/ai-fills — the table's running fills, and
// those that ended in the last day, newest first.
func ListAIFills(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	fills, err := business.AIFills(ctx, id, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": fills})
}

// CreateField POST /tables/{id}/fields
func CreateField(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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

// UpdateField POST /tables/{id}/fields/{fieldId}/update {name?, type?,
// config?, position?, decimal?, group?} changes what it names; the rest keeps
// its stored value.
func UpdateField(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in business.FieldPatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	changed, err := business.UpdateField(ctx, id, fieldId, in, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated", "cells_changed": changed})
}

// PreviewConversion POST /tables/{id}/fields/{fieldId}/convert-preview {type,
// decimal?, group?} counts what changing the field to type would do to its
// cells, over every row, reading numbers in the asker's decimal and group
// signs, without changing anything: {kept, converted, cleared}.
func PreviewConversion(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	fieldId, ok2 := idParam(r, "fieldId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in business.FieldPatch
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	to := ""
	if in.Type != nil {
		to = *in.Type
	}
	counts, err := business.PreviewConversion(ctx, id, fieldId, to, in.NumberSigns, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": counts})
}

// DeleteField DELETE /tables/{id}/fields/{fieldId}
func DeleteField(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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
	ctx := readerCtx(r)
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

// LockView POST /tables/{id}/views/{viewId}/lock {locked}: a manager locks a
// shared view against change, or unlocks it.
func LockView(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	viewId, ok2 := idParam(r, "viewId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in struct {
		Locked bool `json:"locked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.LockView(ctx, id, viewId, in.Locked, actorFrom(r)); err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// PreviewFormula POST /tables/{id}/formula/preview {formula, field_id} — a
// formula being written, checked against the table's fields without saving
// it: what it gives, its values in the first rows, or why it can't be read.
// field_id is the formula field being edited; leave it out for a new one.
func PreviewFormula(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var body struct {
		Formula string `json:"formula"`
		FieldID string `json:"field_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	fieldID := uuid.Nil
	if body.FieldID != "" {
		parsed, err := uuid.Parse(body.FieldID)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid field id"})
			return
		}
		fieldID = parsed
	}
	out, err := business.PreviewFormula(ctx, id, fieldID, body.Formula, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// ListFields GET /tables/{id}/fields: a table's fields, for choosing what a
// rollup adds up there.
func ListFields(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	fields, err := business.ListTableFields(ctx, id, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": fields})
}

// PickRows GET /tables/{id}/rows/pick?q=: rows to link to, by name.
func PickRows(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	rows, err := business.PickRows(ctx, id, r.URL.Query().Get("q"), actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rows})
}

// ChangeLinks POST /tables/{id}/rows/{rowId}/links: links a row to rows, or
// unlinks it, through a field linking to a table.
func ChangeLinks(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	rowId, ok2 := idParam(r, "rowId")
	if !ok || !ok2 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid id"})
		return
	}
	var in struct {
		Field  uuid.UUID   `json:"field"`
		Add    []uuid.UUID `json:"add"`
		Remove []uuid.UUID `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, _, err := business.ChangeLinks(ctx, id, rowId, in.Field, in.Add, in.Remove, actorFrom(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": row})
}

// ActorFrom is the signed-in member as tables' business layer acts for them,
// for the controllers beside this one (Row Query's).
func ActorFrom(r *http.Request) business.Actor { return actorFrom(r) }

// ReaderCtx is the request's context with the reader's zone (?tz=).
func ReaderCtx(r *http.Request) context.Context { return readerCtx(r) }
