package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetTaskRecurrence says how a task repeats; data is null when it doesn't.
// GET /task/recurrence/{task_uuid}
func GetTaskRecurrence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	taskUUID, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task id."})
		return
	}
	task, err := business.GetDgraphTaskInfo(ctx, taskUUID.String(), userInfo.UserDgraphInfo.Uid)
	if errors.Is(err, business.ErrTaskNotVisible) || (err == nil && task == nil) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read the task. Try again."})
		return
	}
	rec, err := business.GetTaskRecurrence(taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetTaskRecurrence err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read the task. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rec})
}

type taskRecurrenceInput struct {
	TaskUUID string `json:"task_uuid"`
	// Rule is RRULE-lite (FREQ=WEEKLY;BYDAY=MO); empty stops the repeat.
	Rule string `json:"rule"`
	// Mode is "schedule" (default) or "completion".
	Mode string `json:"mode"`
	// TZ is the setter's time zone (IANA): the days in the rule are theirs.
	TZ string `json:"tz"`
}

// SetTaskRecurrence makes a task repeat, changes how, or stops it. The same
// people who may change its status may do this.
// POST /task/recurrence {task_uuid, rule, mode, tz}
func SetTaskRecurrence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in taskRecurrenceInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	taskUUID, err := uuid.Parse(in.TaskUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task id."})
		return
	}
	task, err := business.GetDgraphTaskInfo(ctx, taskUUID.String(), userInfo.UserDgraphInfo.Uid)
	if errors.Is(err, business.ErrTaskNotVisible) || (err == nil && task == nil) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read the task. Try again."})
		return
	}
	if task.Project == nil || task.Project.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the project's admins can make its tasks repeat."})
		return
	}
	rec, err := business.SetTaskRecurrence(ctx, task, in.Rule, in.Mode, in.TZ, userInfo.UserPostgresInfo.Id)
	var re *business.RecurrenceError
	if errors.As(err, &re) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": re.Error()})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetTaskRecurrence err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't change the repeat. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rec})
}
