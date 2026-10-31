package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	business "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type moveTaskInput struct {
	Uuid   string `json:"task_uuid"`
	Status string `json:"task_status"`
	// The cards directly above and below the drop; empty at a column's end.
	Before string `json:"before_task_uuid"`
	After  string `json:"after_task_uuid"`
}

// MoveTask POST /task/moveTask: a card dropped on a board, or a row dragged in
// the list. Keeps its place in the project's one order (business/Task
// Reorder), so it is there on the next load, in every view. A board drop also
// sets its status (a built-in key or one of the project's custom statuses);
// a list drag sends none, and is the Task Writer's rank op. Same permission as
// changing the task (Work Access).
func MoveTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var in moveTaskInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	taskUUID, err := uuid.Parse(in.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "task_uuid is not a task id"})
		return
	}
	for _, id := range []string{in.Before, in.After} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "before_task_uuid and after_task_uuid must be task ids"})
			return
		}
	}
	if strings.TrimSpace(in.Status) == "" {
		place, _ := json.Marshal(map[string]string{"before": in.Before, "after": in.After})
		_, _, err := taskWriter.Tasks.Apply(ctx, &userInfo, taskUUID, []taskWriter.Op{{Kind: taskWriter.OpRank, Value: place}}, 0)
		if err != nil {
			writeTaskWriteFailure(w, r, err)
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Moved task"})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, in.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTaskInfo == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	// Moving a card is changing its task (Work Access).
	if !projectaccess.AllowTask(w, r, dgraphTaskInfo, workAccess.NeedEditTask) {
		return
	}

	err = business.MoveTask(ctx, taskUUID, in.Status, in.Before, in.After, dgraphTaskInfo, &userInfo.UserDgraphInfo)
	if projectaccess.RefuseStatus(w, err) {
		return
	}
	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That column is not a status in this project any more. Reload the board."})
		return
	}
	if err != nil {
		// A move that lost to another change of the same tasks, after the
		// write's own retries, is a 409 to try again (a list reorder sends one
		// move per task); anything else is a 503.
		helpers.WriteWriteFailure(w, r, err, "Couldn't move the task.", "controllers/MoveTask")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Moved task"})
}

// writeTaskWriteFailure answers a Task Writer change that failed: a value it
// can't take (400), a task they may not see or change (as the check says it),
// anything else as a failed write, as a board drop's is: a move that lost to
// another change of the same tasks after the write's own retries is a 409 to
// try again, the rest a 503 (helpers.WriteWriteFailure).
func writeTaskWriteFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, taskWriter.ErrBadValue):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That place couldn't be read."})
	case errors.Is(err, workAccess.ErrNotAllowed), errors.Is(err, business.ErrTaskNotVisible):
		projectaccess.WriteTaskRefusal(w, r, err)
	default:
		helpers.WriteWriteFailure(w, r, err, "Couldn't move the task.", "controllers/MoveTask")
	}
}
