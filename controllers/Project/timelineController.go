package controllers

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Project"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// ProjectTimeline handles GET /project/{project_uuid}/timeline: the project's
// tasks as its timeline draws them, for its members. can_edit says whether
// the reader may move them (its admins, as for every task edit); total is how
// many tasks there are, when more than were sent.
func ProjectTimeline(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, false, "")
	if !ok {
		return
	}
	user := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	p, err := business.GetProjectTimeline(r.Context(), id.String(), user.UserDgraphInfo.Uid)
	if err != nil || p == nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The timeline couldn't load just now. Try again in a moment."})
		return
	}
	tasks := p.Tasks
	if tasks == nil {
		tasks = []*dgraphStruct.DgraphTask{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"tasks":    tasks,
		"total":    p.TaskCount,
		"can_edit": p.IsProjectAdmin > 0,
	}})
}
