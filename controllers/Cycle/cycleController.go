package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Cycles: see business/Cycle. Members of a project see its cycles; its
// admins (who also edit its tasks) make, complete and fill them.

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ce *cycleBusiness.CycleError
	if errors.As(err, &ce) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ce.Error()})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/Cycle/%s err: %+v", where, err)
	helpers.WriteRetryLater(w, "Something went wrong.")
}

// project checks the caller may do need with the project: see its cycles, or
// (NeedManage) make, rename, complete and delete them.
func project(w http.ResponseWriter, r *http.Request, need workAccess.Need) (uuid.UUID, *dgraphStruct.DgraphProject, workAccess.Standing, bool) {
	return projectaccess.RequireStanding(w, r, need, "Only the project's admins can change its cycles.")
}

// cycleIn reads the cycle in the URL, which must belong to the project.
func cycleIn(w http.ResponseWriter, r *http.Request, projectID uuid.UUID) (*cycleModel.Cycle, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "cycle_id"))
	if err == nil {
		c, err := cycleModel.Get(id)
		if err != nil {
			write(w, r, "cycleIn", err)
			return nil, false
		}
		if c != nil && c.ProjectUUID == projectID {
			return c, true
		}
	}
	helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Cycle not found"})
	return nil, false
}

// ListCycles is a project's cycles with progress. GET /project/{p}/cycles
func ListCycles(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	views, err := cycleBusiness.List(r.Context(), id, time.Now())
	if err != nil {
		write(w, r, "ListCycles", err)
		return
	}
	schedule, err := cycleBusiness.GetSchedule(r.Context(), id)
	if err != nil {
		write(w, r, "ListCycles", err)
		return
	}
	// can_edit tells the page whether to offer making and completing cycles;
	// schedule, whether they run themselves (B3b).
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"cycles": views, "schedule": schedule, "can_edit": standing.Can(workAccess.NeedManage)}})
}

// CycleBurndown is a cycle's burndown, by day in the reader's zone, and the
// velocity of the project's latest completed cycles.
// GET /project/{p}/cycles/{c}/burndown?tz=
func CycleBurndown(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	c, ok := cycleIn(w, r, id)
	if !ok {
		return
	}
	view, err := cycleBusiness.GetBurndown(r.Context(), c, time.Now(), helpers.Location(r.URL.Query().Get("tz")))
	if err != nil {
		write(w, r, "CycleBurndown", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// CreateCycle adds one. POST /project/{p}/cycles {name, starts_at, weeks}
func CreateCycle(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	var in cycleBusiness.CreateInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	c, err := cycleBusiness.Create(id, in, userInfo.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "CreateCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": c})
}

// RenameCycle. POST /project/{p}/cycles/{c}/rename {name}
func RenameCycle(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	c, ok := cycleIn(w, r, id)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	if err := cycleBusiness.Rename(c.Id, in.Name); err != nil {
		write(w, r, "RenameCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Renamed"})
}

// CompleteCycle closes a cycle. POST /project/{p}/cycles/{c}/complete {carry}
func CompleteCycle(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	c, ok := cycleIn(w, r, id)
	if !ok {
		return
	}
	var in struct {
		Carry bool `json:"carry"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	res, err := cycleBusiness.Complete(r.Context(), c, in.Carry, userInfo.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "CompleteCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// DeleteCycle removes a cycle; its tasks stay, out of any cycle.
// POST /project/{p}/cycles/{c}/delete
func DeleteCycle(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	c, ok := cycleIn(w, r, id)
	if !ok {
		return
	}
	if err := cycleModel.Delete(c.Id); err != nil {
		write(w, r, "DeleteCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// GetTaskCycle is the cycle a task is in (null when none). GET /task/cycle/{task_uuid}
func GetTaskCycle(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_uuid"), workAccess.NeedView)
	if !ok {
		return
	}
	c, err := cycleModel.CycleOf(taskID)
	if err != nil {
		write(w, r, "GetTaskCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": c})
}

// SetTaskCycle puts a task in one of its project's cycles, or takes it out
// (cycle_id ""). POST /task/cycle {task_uuid, cycle_id}
func SetTaskCycle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskUUID string `json:"task_uuid"`
		CycleID  string `json:"cycle_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	// Putting a task in a cycle is changing the task.
	taskID, task, ok := projectaccess.RequireTask(w, r, in.TaskUUID, workAccess.NeedEditTask)
	if !ok {
		return
	}
	if task.Project == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task in no project has no cycles."})
		return
	}
	var cycle *uuid.UUID
	if in.CycleID != "" {
		id, err := uuid.Parse(in.CycleID)
		c, gErr := cycleModel.Get(id)
		if err != nil || gErr != nil || c == nil || c.ProjectUUID.String() != task.Project.Uuid {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't one of this project's cycles."})
			return
		}
		if c.CompletedAt != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That cycle is complete. Pick a current or upcoming one."})
			return
		}
		cycle = &c.Id
	}
	if err := cycleModel.SetTask(taskID, cycle); err != nil {
		write(w, r, "SetTaskCycle", err)
		return
	}
	c, _ := cycleModel.CycleOf(taskID)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": c})
}

// Backlog is the project's backlog (B3): its open cycles with their tasks,
// estimates and the project's capacity, then the open tasks in no cycle.
// can_edit offers starting, completing and making cycles; can_plan moving
// tasks between them (each move is still checked per task).
// GET /project/{p}/backlog
func Backlog(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	b, err := cycleBusiness.GetBacklog(r.Context(), id, time.Now())
	if err != nil {
		write(w, r, "Backlog", err)
		return
	}
	// The schedule, when the project's cycles run themselves (B3b); null when by hand.
	schedule, err := cycleBusiness.GetSchedule(r.Context(), id)
	if err != nil {
		write(w, r, "Backlog", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"backlog": b, "schedule": schedule, "can_edit": standing.Can(workAccess.NeedManage), "can_plan": standing.Can(workAccess.NeedEditTask),
	}})
}

// SetCycleSchedule makes the project's cycles run themselves, or changes how
// (B3b), and makes the cycles it keeps ready at once.
// POST /project/{p}/cycles/schedule {weeks, cooldown_weeks, upcoming, roll_over, starts_at}
func SetCycleSchedule(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	var in cycleBusiness.ScheduleInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	s, err := cycleBusiness.SetSchedule(r.Context(), id, in, userInfo.UserPostgresInfo.Id, time.Now())
	if err != nil {
		write(w, r, "SetCycleSchedule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// StopCycleSchedule hands the project's cycles back to its people; the cycles
// already made stay. POST /project/{p}/cycles/schedule/stop
func StopCycleSchedule(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	if _, err := cycleBusiness.StopSchedule(r.Context(), id); err != nil {
		write(w, r, "StopCycleSchedule", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": nil})
}

// StartCycle starts an upcoming cycle now, keeping its length (B3).
// POST /project/{p}/cycles/{c}/start
func StartCycle(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	c, ok := cycleIn(w, r, id)
	if !ok {
		return
	}
	started, err := cycleBusiness.Start(r.Context(), c, time.Now())
	if err != nil {
		write(w, r, "StartCycle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": started})
}
