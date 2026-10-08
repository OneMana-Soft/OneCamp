package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	business "github.com/akashc777/OneCamp/business/Project"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// workloadWeeks is how many weeks the workload shows unless asked, and the most.
const (
	workloadWeeks    = 12
	maxWorkloadWeeks = 26
)

// Workload handles GET /project/workload?tz=&weeks=: who has how much to do
// each week, across every project the person is in (business.GetWorkload).
// weeks is how many weeks are shown, from this one, in tz; the app places the
// tasks in its own weeks, so a week more is read for the zones' edges.
func Workload(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	weeks, err := strconv.Atoi(r.URL.Query().Get("weeks"))
	if err != nil || weeks < 1 || weeks > maxWorkloadWeeks {
		weeks = workloadWeeks
	}
	now := time.Now().In(helpers.Location(r.URL.Query().Get("tz")))
	// A week back and a week on for the zones' edges; the app keeps to its own weeks.
	from, until := now.AddDate(0, 0, -7), now.AddDate(0, 0, 7*(weeks+1))
	wl, err := business.GetWorkload(r.Context(), user.UserDgraphInfo.Uid, user.UserDgraphInfo.Uuid, user.UserPostgresInfo.IsAdmin, from, until)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Project/Workload err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The workload couldn't load just now. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": wl})
}

// capacityInput is the body of POST /project/workload/capacity.
type capacityInput struct {
	UserUUID string `json:"user_uuid"`
	// TasksPerWeek is how many tasks a week they take on, HoursPerWeek how
	// many hours they work; either may be left out, and 0 puts back its default.
	TasksPerWeek *int `json:"tasks_per_week"`
	HoursPerWeek *int `json:"hours_per_week"`
}

// SetWorkloadCapacity handles POST /project/workload/capacity {user_uuid,
// tasks_per_week}: how many tasks a week someone takes on, as the workload
// measures them. Anyone can set their own, and a workspace admin anyone's.
func SetWorkloadCapacity(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in capacityInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.UserUUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say whose capacity, and how many tasks a week."})
		return
	}
	err := business.SetCapacity(r.Context(), user.UserDgraphInfo.Uuid, user.UserPostgresInfo.IsAdmin, in.UserUUID, business.Capacity{Tasks: in.TasksPerWeek, Hours: in.HoursPerWeek})
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Capacity saved."})
	case errors.Is(err, business.ErrCapacityRange):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A capacity is from 1 to 100 tasks, or 1 to 168 hours, a week."})
	case errors.Is(err, business.ErrCapacityNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only they or a workspace admin can change their capacity."})
	case errors.Is(err, business.ErrPersonNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That person isn't in this workspace."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Project/SetWorkloadCapacity err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The capacity wasn't saved. Try again in a moment."})
	}
}
