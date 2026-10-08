package controllers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/Project"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Report handles GET /project/report?tz=&weeks=&projects=: how work is going
// across the person's live projects (business.GetReport), for the weeks up to
// this one in tz. projects, comma-separated uuids, keeps to those; a project
// the person isn't in is never read, whatever is asked.
func Report(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	q := r.URL.Query()
	weeks, err := strconv.Atoi(q.Get("weeks"))
	if err != nil {
		weeks = business.ReportWeeks
	}
	var only []string
	if p := strings.TrimSpace(q.Get("projects")); p != "" {
		only = strings.Split(p, ",")
	}
	loc := helpers.Location(q.Get("tz"))
	report, err := business.GetReport(r.Context(), user.UserDgraphInfo.Uid, time.Now(), loc, weeks, only)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Project/Report err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The report couldn't load just now. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": report})
}
