package controllers

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Project"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// ProjectTimeline handles GET /project/{project_uuid}/timeline: the project's
// tasks as its timeline draws them, for its members. can_edit says whether
// the reader may move them at all, and project_access which (Work Access:
// with "Members on tasks assigned to them", only their own); total is how
// many tasks there are, when more than were sent.
func ProjectTimeline(w http.ResponseWriter, r *http.Request) {
	id, _, standing, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	p, err := business.GetProjectTimeline(r.Context(), id.String(), user.UserDgraphInfo.Uid)
	if err != nil || p == nil {
		helpers.WriteRetryLater(w, "The timeline couldn't load just now.")
		return
	}
	tasks := p.Tasks
	if tasks == nil {
		tasks = []*dgraphStruct.DgraphTask{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"tasks":          tasks,
		"total":          p.TaskCount,
		"can_edit":       standing.Can(workAccess.NeedEditTask),
		"project_access": standing.Access(),
	}})
}
