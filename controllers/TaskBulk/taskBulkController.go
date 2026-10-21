// Package controllers (TaskBulk) is one change to many tasks as an API (PR2):
// the list's and board's selection, the public API and agents send one
// request, not one per task. business/Task.Bulk reads, checks and writes.
package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// bodyLimit is the most a bulk request may be: 500 ids and a value.
const bodyLimit = 64 << 10

type input struct {
	TaskUUIDs []string `json:"task_uuids"`
	taskBusiness.BulkChange
}

// Bulk POST /task/bulk {task_uuids, op, value}: makes one change to each task
// the caller may make it to, and says per task how it went (changed,
// unchanged, refused with words). 400 for a request that can't be made.
func Bulk(w http.ResponseWriter, r *http.Request) {
	var in input
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	ids := make([]uuid.UUID, 0, len(in.TaskUUIDs))
	for _, raw := range in.TaskUUIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "\"" + raw + "\" isn't a task."})
			return
		}
		ids = append(ids, id)
	}
	who, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	res, err := taskBusiness.Bulk(r.Context(), &who, ids, in.BulkChange)
	var be *taskBusiness.BulkError
	switch {
	case errors.As(err, &be):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": be.Error()})
		return
	case err != nil && res == nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskBulk.Bulk err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change those tasks. Try again.")
		return
	case err != nil:
		// Checked, but the write failed: none of the batched ones changed.
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskBulk.Bulk write err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change those tasks. Try again.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// createMost is the most tasks one createTasks request makes.
const createMost = 100

type newTask struct {
	Name     string `json:"task_name"`
	Priority string `json:"task_priority"`
}

type createInput struct {
	ProjectUUID string    `json:"project_uuid"`
	Tasks       []newTask `json:"tasks"`
}

// CreateTasks POST /task/createTasks {project_uuid, tasks: [{task_name,
// task_priority}]}: makes up to 100 tasks in a project in one request (a
// whiteboard's notes, an assistant's proposals), checked once. They are made
// in order and the first that can't be made stops the rest, so trying again
// makes only those; the answer lists the ids made, in order.
func CreateTasks(w http.ResponseWriter, r *http.Request) {
	var in createInput
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	project, err := uuid.Parse(in.ProjectUUID)
	switch {
	case err != nil:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which project the tasks go in."})
		return
	case len(in.Tasks) == 0:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Name at least one task."})
		return
	case len(in.Tasks) > createMost:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("Make at most %d tasks at a time.", createMost)})
		return
	}
	for i := range in.Tasks {
		in.Tasks[i].Name = strings.TrimSpace(in.Tasks[i].Name)
		if in.Tasks[i].Name == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Every task needs a name."})
			return
		}
		if in.Tasks[i].Priority == "" {
			in.Tasks[i].Priority = dgraphStruct.TASK_PRIORITY_MEDIUM
		}
		if !taskPriority.Is(in.Tasks[i].Priority) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("%q isn't a priority.", in.Tasks[i].Priority)})
			return
		}
	}
	who, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	info, err := projectBusiness.GetBasicDgraphProjectInfo(r.Context(), project.String(), who.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/TaskBulk.CreateTasks")
		return
	}
	// Once for them all, as the single create checks one.
	if _, ok := projectaccess.Allow(w, r, info, workAccess.NeedCreateTask, ""); !ok {
		return
	}
	batch := make([]taskBusiness.NewTask, len(in.Tasks))
	for i, t := range in.Tasks {
		batch[i] = taskBusiness.NewTask{Input: adapter.CreateOrUpdateTaskInput{TaskName: t.Name, ProjectUuid: project.String(), Priority: t.Priority}, Parent: -1}
	}
	// All of them in one go (the BE audit's #30): one statement, one graph write.
	ids, err := taskBusiness.CreateTasks(r.Context(), project, &who, info, batch)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskBulk.CreateTasks err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't make the tasks. Try again.")
		return
	}
	made := make([]string, len(ids))
	for i, id := range ids {
		made[i] = id.String()
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"made": made}})
}
