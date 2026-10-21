package controllers

import (
	"net/http"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
)

// startOfDay is the day now is in, in loc, as its first moment. Pure.
func startOfDay(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// ProjectGlance is a project's open, done, overdue and due-this-week counts,
// over all its tasks the reader may see, in their day (?tz=, an IANA zone).
// GET /project/{project_uuid}/glance
func ProjectGlance(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	g, err := domain.GetProjectGlance(r.Context(), id.String(), startOfDay(time.Now(), helpers.Location(r.URL.Query().Get("tz"))))
	if err != nil {
		helpers.WriteRetryLater(w, "Couldn't count the project's tasks.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": g})
}
