// Package controller (TaskField) serves a project's custom task fields and
// each task's values of them. Any member reads them, and sets values of the
// tasks they may edit; the project's admins make and change fields. A money
// field's values are its project's admins' only, to read and to set
// (V-698): anyone else sees it listed as hidden. See business/TaskField.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	business "github.com/akashc777/OneCamp/business/TaskField"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	rateModel "github.com/akashc777/OneCamp/models/postgres/ProjectRate"
	fieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const adminOnly = "Only the project's admins can change its fields."

func fail(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ie *business.InputError
	switch {
	case errors.As(err, &ie):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ie.Error()})
	case errors.Is(err, business.ErrNameTaken):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "This project already has a field with that name."})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That field no longer exists."})
	case errors.Is(err, business.ErrTaskDeleted):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That task was deleted."})
	case errors.Is(err, business.ErrMoneyAdminsOnly):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": business.MoneyAdminsOnly})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskField/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "That couldn't be saved just now.")
	}
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That change couldn't be read."})
		return false
	}
	return true
}

func fieldID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "field_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That field no longer exists."})
		return uuid.Nil, false
	}
	return id, true
}

func userOf(r *http.Request) userModels.UserInfo {
	return r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
}

// ListFields is a project's fields. GET /project/{p}/fields
func ListFields(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	data, err := Fields(r.Context(), id, standing)
	if err != nil {
		fail(w, r, "ListFields", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": data})
}

// Fields is a project's custom fields as GET /project/{p}/fields answers them,
// for a reader who may see the project; the task panel's
// ?include=project_meta reads the same. can_edit tells the app whether to
// offer making fields; a field hidden from the reader of ctx says so
// (business.Hidden); builtins is the task's own facts a formula may read
// ({Due date}), so the app keeps no list of its own.
func Fields(ctx context.Context, project uuid.UUID, standing workAccess.Standing) (map[string]any, error) {
	fields, err := business.List(ctx, project)
	if err != nil {
		return nil, err
	}
	hidden := business.Hidden(ctx, business.ReaderOf(ctx), fields)
	return map[string]any{"fields": business.Views(hidden, fields...), "can_edit": standing.Can(workAccess.NeedManage), "builtins": business.Builtins()}, nil
}

// CreateField adds one. POST /project/{p}/fields {name, type, options, currency, on_card, formula}
func CreateField(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, adminOnly)
	if !ok {
		return
	}
	var in business.Input
	if !decode(w, r, &in) {
		return
	}
	currency := ""
	if billing, err := rateModel.Get(id); err == nil && billing != nil {
		currency = billing.Currency
	}
	f, err := business.Create(r.Context(), id, in, userOf(r).UserPostgresInfo.Id, currency)
	if err != nil {
		fail(w, r, "CreateField", err)
		return
	}
	view, err := business.ViewOf(r.Context(), f)
	if err != nil {
		fail(w, r, "CreateField", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// PreviewField checks a formula as a project admin writes it, without
// saving it: what it gives, or why it can't be read; its values show on
// the tasks once it's saved (values is empty). POST
// /project/{p}/fields/preview {formula, field_id (none for a new field)}
func PreviewField(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, adminOnly)
	if !ok {
		return
	}
	var in struct {
		Formula string `json:"formula"`
		FieldID string `json:"field_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	fid := uuid.Nil
	if in.FieldID != "" {
		parsed, err := uuid.Parse(in.FieldID)
		if err != nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That field no longer exists."})
			return
		}
		fid = parsed
	}
	out, err := business.Preview(r.Context(), id, fid, in.Formula)
	if err != nil {
		fail(w, r, "PreviewField", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// UpdateField renames a field, changes its options or currency, or whether
// cards show it. POST /project/{p}/fields/{f}
func UpdateField(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, adminOnly)
	if !ok {
		return
	}
	fid, ok := fieldID(w, r)
	if !ok {
		return
	}
	var in business.Input
	if !decode(w, r, &in) {
		return
	}
	f, changed, err := business.Update(r.Context(), id, fid, in)
	if err != nil {
		fail(w, r, "UpdateField", err)
		return
	}
	view, err := business.ViewOf(r.Context(), f)
	if err != nil {
		fail(w, r, "UpdateField", err)
		return
	}
	// changed: the tasks an option taken away came off, for the app to refresh.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"field": view, "changed": len(changed)}})
}

// ReorderFields puts the fields in the order given. POST /project/{p}/fields/reorder {ids}
func ReorderFields(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, adminOnly)
	if !ok {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	ids := make([]uuid.UUID, 0, len(in.IDs))
	for _, raw := range in.IDs {
		if fid, err := uuid.Parse(raw); err == nil {
			ids = append(ids, fid)
		}
	}
	if err := business.Reorder(r.Context(), id, ids); err != nil {
		fail(w, r, "ReorderFields", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Fields reordered."})
}

// DeleteField removes a field and every task's value of it.
// POST /project/{p}/fields/{f}/delete
func DeleteField(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, adminOnly)
	if !ok {
		return
	}
	fid, ok := fieldID(w, r)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), id, fid, userOf(r).UserPostgresInfo.Id); err != nil {
		fail(w, r, "DeleteField", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Field deleted."})
}

// SetTaskField gives a task its value of a field, or takes it off (value
// null). POST /task/field {task_uuid, field_id, value}
func SetTaskField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userOf(r)
	var in struct {
		TaskUUID string          `json:"task_uuid"`
		FieldID  string          `json:"field_id"`
		Value    json.RawMessage `json:"value"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := uuid.Parse(in.TaskUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	fid, err := uuid.Parse(in.FieldID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That field no longer exists."})
		return
	}
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, in.TaskUUID, user.UserDgraphInfo.Uid)
	if err != nil || task == nil || task.Uuid == "" || task.Project == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	if !projectaccess.AllowTask(w, r, task, workAccess.NeedEditTask) {
		return
	}
	projectID, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	f, err := fieldModel.Get(ctx, projectID, fid)
	if err == nil {
		err = business.MaySet(ctx, business.ReaderOf(ctx), f)
	}
	if err != nil {
		fail(w, r, "SetTaskField", err)
		return
	}
	value, err := business.SetValue(ctx, task, f, in.Value, &user.UserDgraphInfo, user.UserPostgresInfo.Id)
	if err != nil {
		fail(w, r, "SetTaskField", err)
		return
	}
	told, refetch := business.ForEveryone(f, value)
	// A formula reading the field changes with it: each app reads the task
	// again, as itself, for the formulas' new values (refetch).
	formulas := business.FormulasRead(ctx, f)
	mqttBusiness.PublishTaskField(&mqttStruct.MqttTaskField{TaskUuid: task.Uuid, ProjectUuid: task.Project.Uuid, FieldID: f.ID.String(), Value: told, Refetch: refetch || formulas, By: user.UserDgraphInfo.Uuid})
	data := map[string]any{"task_uuid": task.Uuid, "field_id": f.ID, "value": value}
	if formulas {
		data["refetch"] = true
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": data})
}
