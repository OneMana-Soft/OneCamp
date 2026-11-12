package controller

// The work hierarchy's routes (jira-01 J-H): a task goes under an epic or a
// task at the top of its project, or out to the top, on the one parent/
// sub-task relation, through the Task Writer (business/TaskWriter OpParent).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// parentInput is the body of POST /task/parent.
type parentInput struct {
	Uuid string `json:"task_uuid"`
	// Parent is the task it goes under; "" takes it out to the top.
	Parent string `json:"parent_task_uuid"`
}

// SetTaskParent handles POST /task/parent {task_uuid, parent_task_uuid}: the
// task goes under the parent named (an epic, or a task at the top or in an
// epic, of its own project), or with "" out to the top. The person must be
// able to change the task and see the parent; an epic never goes under
// anything, and a sub-task holds nothing.
func SetTaskParent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in parentInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That change couldn't be read."})
		return
	}
	taskID, err := uuid.Parse(in.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	if _, err := uuid.Parse(in.Parent); in.Parent != "" && err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	value, _ := json.Marshal(in.Parent)
	_, _, err = taskWriter.Tasks.Apply(ctx, &userInfo, taskID, []taskWriter.Op{{Kind: taskWriter.OpParent, Value: value}}, 0)
	writeHierarchyResult(w, r, err, "controllers/SetTaskParent")
}

// writeHierarchyResult answers a change to the hierarchy or a task's links:
// a refusal in its words, a task that isn't there for the person as not
// found (nothing says whether it exists).
func writeHierarchyResult(w http.ResponseWriter, r *http.Request, err error, where string) {
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved."})
	case errors.Is(err, taskWriter.ErrBadValue):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
	case errors.Is(err, business.ErrHierarchy), errors.Is(err, business.ErrLink):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, business.ErrNotAllowed):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, helpers.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
	default:
		helpers.LogErrorWithContext(r.Context(), "%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "That couldn't be saved just now.")
	}
}
