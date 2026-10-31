package controller

// The critical path and the impact line (DG10): reads for the timeline. The
// answers' shapes are design/dgraph/DG10-CONTRACT.md's.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CriticalPath handles GET /project/{project_uuid}/critical-path?tz=: the
// project's critical path and each task's slack, in the reader's days, with
// linked tasks of other projects they may see. For whoever may view the
// project.
func CriticalPath(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	ctx := r.Context()
	answer, err := business.CriticalPath(ctx, id.String(), helpers.Location(r.URL.Query().Get("tz")), &user.UserDgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CriticalPath err: %+v", err)
		helpers.WriteRetryLater(w, "The critical path couldn't load just now.")
		return
	}
	answer.CanEdit = standing.Can(workAccess.NeedEditTask)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
}

// ProjectForecast handles GET /project/{project_uuid}/forecast?tz=: when the
// project's open work lands, from its pace and from what it waits on
// (linear-20), in the reader's days. For whoever may view the project, over
// the tasks they may see.
func ProjectForecast(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	ctx := r.Context()
	answer, err := business.Forecast(ctx, id.String(), time.Now(), helpers.Location(r.URL.Query().Get("tz")), &user.UserDgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ProjectForecast err: %+v", err)
		helpers.WriteRetryLater(w, "The forecast couldn't load just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
}

// forecastsCap is the most projects one forecasts read takes. More is refused
// whole (400), so a client with more asks in pages of this many.
const forecastsCap = 100

// forecastIDs reads ?ids= (comma-separated): each project once, in the order
// asked, and a string that isn't an id left out. ok is false when more than
// forecastsCap distinct ids were asked for; a repeat doesn't count.
func forecastIDs(raw string) (ids []uuid.UUID, ok bool) {
	asked := map[string]bool{}
	have := map[uuid.UUID]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || asked[part] {
			continue
		}
		asked[part] = true
		if len(asked) > forecastsCap {
			return nil, false
		}
		if id, err := uuid.Parse(part); err == nil && !have[id] {
			have[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

// ProjectForecasts handles GET /project/forecasts?ids=&tz=: the listed
// projects' forecasts in one read (linear-20: the projects overview and the
// goal rows), keyed by project id. Each is the single route's own forecast
// (business.Forecasts: the same function over the same plan read), so the two
// can't drift, and the reads are the same few however many projects are
// listed. Only projects the caller may see answer, by the single route's
// check (NeedView); the rest, and ids that name nothing, are left out
// without a word.
func ProjectForecasts(w http.ResponseWriter, r *http.Request) {
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	ids, ok := forecastIDs(r.URL.Query().Get("ids"))
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("Ask for at most %d projects' forecasts at once.", forecastsCap)})
		return
	}
	asked := make([]string, len(ids))
	for i, id := range ids {
		asked[i] = id.String()
	}
	ctx := r.Context()
	forecasts, err := business.Forecasts(ctx, asked, time.Now(), helpers.Location(r.URL.Query().Get("tz")), &user.UserDgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ProjectForecasts err: %+v", err)
		helpers.WriteRetryLater(w, "The forecasts couldn't load just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"forecasts": forecasts}})
}

// TaskImpact handles GET /task/{task_uuid}/impact?start=&due=&tz=: what moving
// the task to those dates would push along, before the move is made. Nothing
// is written. For whoever may change the task.
func TaskImpact(w http.ResponseWriter, r *http.Request) {
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	ctx := r.Context()
	taskUUID, err := uuid.Parse(chi.URLParam(r, "task_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return
	}
	task, err := business.GetDgraphBasicTaskInfo(ctx, taskUUID.String(), user.UserDgraphInfo.Uid)
	if err != nil || task == nil || task.Uuid == "" || task.Project == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
		return
	}
	if !projectaccess.AllowTask(w, r, task, workAccess.NeedEditTask) {
		return
	}
	q := r.URL.Query()
	start, startErr := dateOrUnset(q.Get("start"))
	due, dueErr := dateOrUnset(q.Get("due"))
	if startErr != nil || dueErr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Dates are written like 2026-10-12T17:00:00Z."})
		return
	}
	if start.Year() > 1970 && due.Year() > 1970 && start.After(*due) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A task can't start after it's due."})
		return
	}
	answer, err := business.Impact(ctx, task, business.TaskDates{Start: start, Due: due}, helpers.Location(q.Get("tz")), &user.UserDgraphInfo)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
	case errors.Is(err, business.ErrTaskDeleted), errors.Is(err, business.ErrDependencyMissing):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Task not found"})
	case errors.Is(err, business.ErrNotAllowed):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/TaskImpact err: %+v", err)
		helpers.WriteRetryLater(w, "That couldn't be worked out just now.")
	}
}
