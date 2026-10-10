// Package controller (TimeEntry) is time on tasks: a timer a person starts and
// stops, time added by hand, and a project's report for invoicing. Anyone who
// can see a task can log time on it; people change only their own entries.
package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	timeBusiness "github.com/akashc777/OneCamp/business/TimeTracking"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	rateModel "github.com/akashc777/OneCamp/models/postgres/ProjectRate"
	timeModel "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// EntryView is an entry as the app shows it: who, and for how long so far.
type EntryView struct {
	timeModel.Entry
	Person  string `json:"person"`
	Seconds int64  `json:"seconds"`
	Mine    bool   `json:"mine"`
}

func me(r *http.Request) userModels.UserInfo {
	return r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
}

func fail(w http.ResponseWriter, r *http.Request, where string, err error) {
	var in *timeBusiness.InputError
	if errors.As(err, &in) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": in.Msg})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/TimeEntry/%s err: %+v", where, err)
	helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't save that. Try again."})
}

// peopleNames is display names for user ids (Postgres id == graph uuid), in
// one lookup. Someone no longer in the workspace is absent.
func peopleNames(r *http.Request, ids map[uuid.UUID]bool) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id.String())
	}
	users, err := userDomain.GetUserDisplayMapByUUIDs(r.Context(), list)
	if err != nil {
		return out
	}
	for raw, u := range users {
		id, err := uuid.Parse(raw)
		if err != nil || u == nil {
			continue
		}
		out[id] = u.DisplayName()
	}
	return out
}

func views(r *http.Request, entries []timeModel.Entry, now time.Time) []EntryView {
	ids := map[uuid.UUID]bool{}
	for _, e := range entries {
		ids[e.UserID] = true
	}
	names := peopleNames(r, ids)
	mine := me(r).UserPostgresInfo.Id
	out := make([]EntryView, 0, len(entries))
	for _, e := range entries {
		person := names[e.UserID]
		if person == "" {
			person = "Former member"
		}
		out = append(out, EntryView{Entry: e, Person: person, Seconds: e.Seconds(now), Mine: e.UserID == mine})
	}
	return out
}

// taskProject is the project a task belongs to, which every entry records so
// reports never need the graph to find it.
func taskProject(w http.ResponseWriter, r *http.Request, raw string) (uuid.UUID, uuid.UUID, bool) {
	taskID, task, ok := projectaccess.RequireTask(w, r, raw)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	if task.Project == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Time goes on a project's tasks."})
		return uuid.Nil, uuid.Nil, false
	}
	projectID, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Time goes on a project's tasks."})
		return uuid.Nil, uuid.Nil, false
	}
	return taskID, projectID, true
}

// GetTaskTime is a task's time and the caller's running timer.
// GET /task/time/{task_uuid}
func GetTaskTime(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_uuid"))
	if !ok {
		return
	}
	entries, err := timeModel.ForTask(taskID)
	if err != nil {
		fail(w, r, "GetTaskTime", err)
		return
	}
	now := time.Now()
	var total, billable int64
	for i := range entries {
		s := entries[i].Seconds(now)
		total += s
		if entries[i].Billable {
			billable += s
		}
	}
	running, err := timeModel.Running(me(r).UserPostgresInfo.Id)
	if err != nil {
		fail(w, r, "GetTaskTime", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"entries":          views(r, entries, now),
		"seconds":          total,
		"billable_seconds": billable,
		"running":          running,
	}})
}

// RunningTimer is the caller's running timer and its task's name, or null.
// GET /task/time/running
func RunningTimer(w http.ResponseWriter, r *http.Request) {
	u := me(r)
	e, err := timeModel.Running(u.UserPostgresInfo.Id)
	if err != nil {
		fail(w, r, "RunningTimer", err)
		return
	}
	if e == nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": nil})
		return
	}
	name := "A task"
	if t, err := taskBusiness.GetDgraphBasicTaskInfo(r.Context(), e.TaskUUID.String(), u.UserDgraphInfo.Uid); err == nil && t != nil && t.Name != "" {
		name = t.Name
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"entry": e, "task_name": name, "seconds": e.Seconds(time.Now()),
	}})
}

// StartTimer starts the caller's timer on a task, stopping any other.
// POST /task/time/{task_uuid}/start
func StartTimer(w http.ResponseWriter, r *http.Request) {
	taskID, projectID, ok := taskProject(w, r, chi.URLParam(r, "task_uuid"))
	if !ok {
		return
	}
	started, stopped, err := timeModel.Start(me(r).UserPostgresInfo.Id, taskID, projectID, time.Now().UTC())
	if err != nil {
		fail(w, r, "StartTimer", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"started": started, "stopped": stopped}})
}

// StopTimer stops the caller's running timer.
// POST /task/time/stop
func StopTimer(w http.ResponseWriter, r *http.Request) {
	e, err := timeModel.Stop(me(r).UserPostgresInfo.Id, time.Now().UTC())
	if err != nil {
		fail(w, r, "StopTimer", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"stopped": e}})
}

type spanInput struct {
	StartedAt time.Time `json:"started_at"`
	Minutes   int       `json:"minutes"`
	Note      string    `json:"note"`
	Billable  *bool     `json:"billable"`
}

func readSpan(w http.ResponseWriter, r *http.Request) (spanInput, bool) {
	var in spanInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return in, false
	}
	return in, true
}

// AddTime records time worked on a task, entered by hand.
// POST /task/time/{task_uuid} {started_at, minutes, note, billable}
func AddTime(w http.ResponseWriter, r *http.Request) {
	taskID, projectID, ok := taskProject(w, r, chi.URLParam(r, "task_uuid"))
	if !ok {
		return
	}
	in, ok := readSpan(w, r)
	if !ok {
		return
	}
	start, end, note, err := timeBusiness.CheckSpan(in.StartedAt, in.Minutes, in.Note, time.Now())
	if err != nil {
		fail(w, r, "AddTime", err)
		return
	}
	e, err := timeModel.Add(timeModel.Entry{
		TaskUUID: taskID, ProjectUUID: projectID, UserID: me(r).UserPostgresInfo.Id,
		StartedAt: start, EndedAt: &end, Note: note, Billable: in.Billable == nil || *in.Billable,
	})
	if err != nil {
		fail(w, r, "AddTime", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// UpdateTime changes one of the caller's finished entries.
// POST /task/time/entry/{entry_id}/update {started_at, minutes, note, billable}
func UpdateTime(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "entry_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't an entry."})
		return
	}
	in, ok := readSpan(w, r)
	if !ok {
		return
	}
	start, end, note, err := timeBusiness.CheckSpan(in.StartedAt, in.Minutes, in.Note, time.Now())
	if err != nil {
		fail(w, r, "UpdateTime", err)
		return
	}
	e, err := timeModel.Update(id, me(r).UserPostgresInfo.Id, start, end, note, in.Billable == nil || *in.Billable)
	if err != nil {
		fail(w, r, "UpdateTime", err)
		return
	}
	if e == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That entry isn't yours to change, or its timer is still running."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// DeleteTime removes one of the caller's entries.
// POST /task/time/entry/{entry_id}/delete
func DeleteTime(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "entry_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't an entry."})
		return
	}
	ok, err := timeModel.Delete(id, me(r).UserPostgresInfo.Id)
	if err != nil {
		fail(w, r, "DeleteTime", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That entry isn't yours to delete."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// ProjectTime is a project's time over a range, by person and by task, or
// every entry as a CSV file for an invoice. Any member may read it; its
// admins also see what the billable time comes to at the project's rates.
// GET /project/{project_uuid}/time?from=&to=[&format=csv&tz=Area/City]
func ProjectTime(w http.ResponseWriter, r *http.Request) {
	projectID, project, ok := projectaccess.Require(w, r, false, "")
	if !ok {
		return
	}
	// Money is for the project's admins; everyone else reads hours.
	rates := ratesFor(r, projectID, project.IsProjectAdmin > 0)
	now := time.Now()
	q := r.URL.Query()
	from, to, err := timeBusiness.ReportRange(q.Get("from"), q.Get("to"), now)
	if err != nil {
		fail(w, r, "ProjectTime", err)
		return
	}
	entries, err := timeModel.ForProject(projectID, from, to)
	if err != nil {
		fail(w, r, "ProjectTime", err)
		return
	}
	ids := map[uuid.UUID]bool{}
	for _, e := range entries {
		ids[e.UserID] = true
	}
	names := timeBusiness.Names{People: peopleNames(r, ids), Tasks: map[uuid.UUID]string{}}
	if len(entries) > 0 {
		if p, err := projectBusiness.GetDgraphProjectTaskListForKanban(r.Context(), projectID.String(), me(r).UserDgraphInfo.Uid, "", 0); err == nil && p != nil {
			for _, col := range [][]*dgraphStruct.DgraphTask{p.TasksTodo, p.TasksInProgresss, p.TasksBacklog, p.TasksInReview, p.TasksCanceled, p.TasksDone} {
				for _, t := range col {
					if id, err := uuid.Parse(t.Uuid); err == nil {
						names.Tasks[id] = t.Name
					}
				}
			}
		}
	}
	if q.Get("format") == "csv" {
		loc, err := time.LoadLocation(q.Get("tz"))
		if err != nil || q.Get("tz") == "" {
			loc = time.UTC
		}
		if len(entries) > timeModel.MaxReportRows {
			entries = entries[:timeModel.MaxReportRows]
		}
		file := fmt.Sprintf("%s time %s to %s.csv", safeFileName(project.Name), from.In(loc).Format("2006-01-02"), to.Add(-time.Second).In(loc).Format("2006-01-02"))
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file))
		if err := timeBusiness.WriteCSV(w, entries, names, loc, now, rates); err != nil {
			helpers.LogErrorWithContext(r.Context(), "controllers/TimeEntry/ProjectTime csv err: %+v", err)
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": timeBusiness.Summarise(entries, names, from, to, now, rates)})
}

// ratesFor is a project's rates for a reader who may see money: its admins.
// Everyone else's report has hours only. A failed read leaves money out
// rather than failing the report.
func ratesFor(r *http.Request, projectID uuid.UUID, isAdmin bool) *timeBusiness.Rates {
	if !isAdmin {
		return nil
	}
	b, err := rateModel.Get(projectID)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/TimeEntry/ratesFor err: %+v", err)
		return nil
	}
	if b == nil {
		return nil
	}
	return &timeBusiness.Rates{Currency: b.Currency, Default: b.DefaultRateCents, People: b.People}
}

type ratesView struct {
	Set              bool   `json:"set"`
	Currency         string `json:"currency,omitempty"`
	DefaultRateCents int64  `json:"default_rate_cents"`
	People           []struct {
		UserUUID  string `json:"user_uuid"`
		RateCents int64  `json:"rate_cents"`
	} `json:"people"`
}

func viewOf(rates *timeBusiness.Rates) ratesView {
	v := ratesView{People: []struct {
		UserUUID  string `json:"user_uuid"`
		RateCents int64  `json:"rate_cents"`
	}{}}
	if rates == nil {
		return v
	}
	v.Set, v.Currency, v.DefaultRateCents = true, rates.Currency, rates.Default
	for id, cents := range rates.People {
		v.People = append(v.People, struct {
			UserUUID  string `json:"user_uuid"`
			RateCents int64  `json:"rate_cents"`
		}{id.String(), cents})
	}
	// In a steady order: a map's isn't, and a client comparing two reads
	// would take the same rates for a change.
	sort.Slice(v.People, func(i, j int) bool { return v.People[i].UserUUID < v.People[j].UserUUID })
	return v
}

// ProjectRates is what the project's time is billed at. Its admins only.
// GET /project/{project_uuid}/rates
func ProjectRates(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, true, "Only the project's admins see its rates.")
	if !ok {
		return
	}
	b, err := rateModel.Get(projectID)
	if err != nil {
		fail(w, r, "ProjectRates", err)
		return
	}
	var rates *timeBusiness.Rates
	if b != nil {
		rates = &timeBusiness.Rates{Currency: b.Currency, Default: b.DefaultRateCents, People: b.People}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": viewOf(rates)})
}

// SetProjectRates sets the project's currency and rates. Its admins only.
// POST /project/{project_uuid}/rates {currency, default_rate_cents, people: [{user_uuid, rate_cents}]}
func SetProjectRates(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, true, "Only the project's admins set its rates.")
	if !ok {
		return
	}
	var in timeBusiness.RatesInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read those rates."})
		return
	}
	rates, err := timeBusiness.CheckRates(in)
	if err != nil {
		fail(w, r, "SetProjectRates", err)
		return
	}
	if err := rateModel.Set(projectID, rates.Currency, rates.Default, rates.People, me(r).UserPostgresInfo.Id); err != nil {
		fail(w, r, "SetProjectRates", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": viewOf(rates)})
}

// ClearProjectRates stops billing the project. Its admins only.
// POST /project/{project_uuid}/rates/delete
func ClearProjectRates(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, true, "Only the project's admins change its rates.")
	if !ok {
		return
	}
	if err := rateModel.Clear(projectID); err != nil {
		fail(w, r, "ClearProjectRates", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": viewOf(nil)})
}

// safeFileName keeps a project's name usable in a download's file name.
func safeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`"\/:*?<>|`, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return "Project"
	}
	if r := []rune(name); len(r) > 60 {
		name = string(r[:60])
	}
	return name
}
