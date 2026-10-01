package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	business "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
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

// MoveTask POST /task/moveTask: a card dropped on a board. Sets its status and
// keeps its place in the column, so it is still there on the next load.
// The status is a built-in key or one of the project's custom statuses.
// Same permission as changing a task's status: project admins.
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
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "task_status is required"})
		return
	}

	dgraphTaskInfo, err := business.GetDgraphBasicTaskInfo(ctx, in.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTaskInfo == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	if dgraphTaskInfo.Project == nil || dgraphTaskInfo.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only project admins can move tasks"})
		return
	}

	err = business.MoveTask(ctx, taskUUID, in.Status, in.Before, in.After, dgraphTaskInfo, &userInfo.UserDgraphInfo)
	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That column is not a status in this project any more. Reload the board."})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/MoveTask failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to move the task"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Moved task"})
}
