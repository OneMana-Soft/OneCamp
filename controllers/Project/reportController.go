package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Report handles GET /project/report?tz=&weeks=&projects=&filters=
// &taskSearchString=: how work is going across the person's live projects
// (business.ReportFor), for the weeks up to this one in tz. projects,
// comma-separated uuids, keeps to those; a project the person isn't in is
// never read, whatever is asked. filters and taskSearchString are a view's,
// as its list sends them (B9-b1), through the list's own builder: with one
// project the person reads, its side filters (cycles, custom fields, types)
// too, as on its list; across several, none, as on a person's own list.
func Report(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	q := r.URL.Query()
	weeks, err := strconv.Atoi(q.Get("weeks"))
	if err != nil {
		weeks = business.ReportWeeks
	}
	var only []string
	if p := strings.TrimSpace(q.Get("projects")); p != "" {
		only = strings.Split(p, ",")
	}
	var params []taskDomain.TaskListParam
	if raw := q.Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &params); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse filters query param"})
			return
		}
	}
	// The zone on ctx too: a formula field's TODAY() is the reader's day.
	report, err := business.ReportFor(helpers.WithZone(ctx, q.Get("tz")), user.UserDgraphInfo.Uid, time.Now(), helpers.Location(q.Get("tz")),
		business.ReportAsk{Weeks: weeks, Only: only, Params: params, Search: q.Get("taskSearchString"), Reader: user.UserPostgresInfo.Id.String()})
	if errors.Is(err, taskDomain.ErrSearchTerm) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search string is too long or invalid"})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Project/Report err: %+v", err)
		helpers.WriteRetryLater(w, "The report couldn't load just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": report})
}
