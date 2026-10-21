// Package controllers (TaskKeys) is the HTTP side of task keys and types
// (projects A8K): a project's key and the keys it had, its types, a task's
// type, and finding a task by its key.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// bodyLimit is the most a request here may send.
const bodyLimit = 4 << 10

func caller(r *http.Request) userModels.UserInfo {
	if u, ok := httpx.UserFrom(r); ok && u != nil {
		return *u
	}
	return userModels.UserInfo{}
}

// keyAnswer is a project's key as the app gets it.
type keyAnswer struct {
	Key        string   `json:"key"`
	NextNumber int      `json:"next_number"`
	Aliases    []string `json:"aliases"`
}

// ProjectKey GET /project/{project_uuid}/key: the project's key, the number
// its next task takes and the keys it used before. Anyone in the project.
func ProjectKey(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	key, next, aliases, err := taskKeys.Of(r.Context(), id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectKey err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't read the project's key.")
		return
	}
	if aliases == nil {
		aliases = []string{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": keyAnswer{Key: key, NextNumber: next, Aliases: aliases}})
}

// SetProjectKey PUT /project/{project_uuid}/key {key}: the project's admins
// change its key. Its tasks are renumbered nowhere: Q4-12 becomes LAUNCH-12,
// and Q4-12 keeps finding it.
func SetProjectKey(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its key.")
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	me := caller(r)
	old, now, err := taskKeys.SetKey(r.Context(), id, in.Key, me.UserPostgresInfo.Id)
	switch {
	case errors.Is(err, taskKeys.ErrBadKey):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A key is 2 to 5 letters and digits, and starts with a letter."})
		return
	case errors.Is(err, taskKeys.ErrKeyTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": now + " is taken by another project. Try another key."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/SetProjectKey err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change the project's key.")
		return
	}
	if old != now {
		auditBusiness.Record(r, "project.key.changed", auditBusiness.CategorySettings,
			"Changed a project's key from "+old+" to "+now,
			map[string]interface{}{"project_uuid": id.String(), "from": old, "to": now})
		resourceEventBusiness.Bus.Emit(r.Context(), resourceEventBusiness.Event{
			Kind: resourceEventBusiness.ProjectChanged, ResourceKind: workAccess.KindProject, ResourceUUID: id.String(),
			Actor: resourceEventBusiness.Actor{UserUUID: me.UserDgraphInfo.Uuid}, At: time.Now(),
			Data: map[string]string{"key": now, "key_was": old},
		})
	}
	ProjectKey(w, r)
}

// typeAnswer is a type as the app gets it: built in, or the project's own.
type typeAnswer struct {
	ID      string `json:"id"`
	Builtin string `json:"builtin,omitempty"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Own     bool   `json:"own,omitempty"`
}

// ProjectTypes GET /project/{project_uuid}/types: the types a task in the
// project can have, and the icons a type of its own may take.
func ProjectTypes(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	types, err := workTaxonomy.Types(r.Context(), id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectTypes err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't read the project's types.")
		return
	}
	out := make([]typeAnswer, 0, len(types))
	for _, t := range types {
		out = append(out, typeAnswer{ID: t.ID.String(), Builtin: t.Builtin, Name: t.Name, Icon: t.Icon, Own: t.ProjectID != nil})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out, "icons": workTaxonomy.Icons})
}

type typeInput struct {
	Name string `json:"name"`
	Icon string `json:"icon"`
}

func readType(w http.ResponseWriter, r *http.Request) (typeInput, bool) {
	var in typeInput
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return in, false
	}
	return in, true
}

// writeTypeRefusal answers a type change that didn't happen, in words.
func writeTypeRefusal(w http.ResponseWriter, r *http.Request, err error, where string) {
	switch {
	case errors.Is(err, workTaxonomy.ErrBadType):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": strings.TrimPrefix(err.Error(), workTaxonomy.ErrBadType.Error()+": ")})
	case errors.Is(err, workTaxonomy.ErrTypeNameTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "The project already has a type of that name."})
	case errors.Is(err, workTaxonomy.ErrNoType):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That type isn't one of this project's. Reload to see its types."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Couldn't save the type.")
	}
}

// CreateProjectType POST /project/{project_uuid}/types {name, icon}: the
// project's admins add a type.
func CreateProjectType(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins add its types.")
	if !ok {
		return
	}
	in, ok := readType(w, r)
	if !ok {
		return
	}
	t, err := workTaxonomy.Create(r.Context(), id, in.Name, in.Icon, caller(r).UserPostgresInfo.Id)
	if err != nil {
		writeTypeRefusal(w, r, err, "CreateProjectType")
		return
	}
	auditBusiness.Record(r, "project.type.created", auditBusiness.CategorySettings, "Added the type "+t.Name+" to a project",
		map[string]interface{}{"project_uuid": id.String(), "type_id": t.ID.String(), "name": t.Name})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": typeAnswer{ID: t.ID.String(), Name: t.Name, Icon: t.Icon, Own: true}})
}

// UpdateProjectType PUT /project/{project_uuid}/types/{type_id} {name, icon}.
func UpdateProjectType(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins change its types.")
	if !ok {
		return
	}
	typeID, err := uuid.Parse(chi.URLParam(r, "type_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That type isn't one of this project's. Reload to see its types."})
		return
	}
	in, ok := readType(w, r)
	if !ok {
		return
	}
	if err := workTaxonomy.Update(r.Context(), id, typeID, in.Name, in.Icon); err != nil {
		writeTypeRefusal(w, r, err, "UpdateProjectType")
		return
	}
	auditBusiness.Record(r, "project.type.changed", auditBusiness.CategorySettings, "Changed a project's type",
		map[string]interface{}{"project_uuid": id.String(), "type_id": typeID.String(), "name": strings.TrimSpace(in.Name)})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved"})
}

// ArchiveProjectType DELETE /project/{project_uuid}/types/{type_id}: out of
// the pickers; its tasks keep it.
func ArchiveProjectType(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins archive its types.")
	if !ok {
		return
	}
	typeID, err := uuid.Parse(chi.URLParam(r, "type_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That type isn't one of this project's. Reload to see its types."})
		return
	}
	if err := workTaxonomy.Archive(r.Context(), id, typeID); err != nil {
		writeTypeRefusal(w, r, err, "ArchiveProjectType")
		return
	}
	auditBusiness.Record(r, "project.type.archived", auditBusiness.CategorySettings, "Archived a project's type",
		map[string]interface{}{"project_uuid": id.String(), "type_id": typeID.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Archived"})
}

// SetTaskType POST /task/type {task_uuid, type_id}: whoever may change the
// task gives it a type; an empty type_id makes it a plain Task. task_uuid may
// be the task's key.
func SetTaskType(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskUUID string `json:"task_uuid"`
		TypeID   string `json:"type_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	_, task, ok := projectaccess.RequireTask(w, r, in.TaskUUID, workAccess.NeedEditTask)
	if !ok {
		return
	}
	var typeID *uuid.UUID
	if s := strings.TrimSpace(in.TypeID); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeTypeRefusal(w, r, workTaxonomy.ErrNoType, "SetTaskType")
			return
		}
		typeID = &id
	}
	me := caller(r)
	now, err := workTaxonomy.SetTaskType(r.Context(), task, typeID, &me.UserDgraphInfo)
	if err != nil {
		writeTypeRefusal(w, r, err, "SetTaskType")
		return
	}
	var answer *dgraphStruct.TaskWorkType
	if now != nil {
		answer = &dgraphStruct.TaskWorkType{ID: now.ID.String(), Builtin: now.Builtin, Name: now.Name, Icon: now.Icon}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"task_uuid": task.Uuid, "task_work_type": answer}})
}

// ResolveTask GET /task/resolve/{ref}: the task a key ("Q4-12") or an id
// names, for the task's own page at /app/task/Q4-12. A task the caller can't
// open answers exactly as one that doesn't exist.
func ResolveTask(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	notFound := func() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no task " + strings.ToUpper(strings.TrimSpace(ref)) + " that you can open."})
	}
	id, err := taskKeys.Ref(r.Context(), ref)
	if errors.Is(err, taskKeys.ErrNoTask) {
		notFound()
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ResolveTask err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't look that task up.")
		return
	}
	me := caller(r)
	task, err := taskBusiness.GetDgraphTaskInfo(r.Context(), id.String(), me.UserDgraphInfo.Uid)
	if errors.Is(err, taskBusiness.ErrTaskNotVisible) || errors.Is(err, helpers.ErrNotFound) || (err == nil && (task == nil || task.Uuid == "")) {
		notFound()
		return
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "task", "controllers/ResolveTask")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"task_uuid": task.Uuid, "task_key": task.Key}})
}
