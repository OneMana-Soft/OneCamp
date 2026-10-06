package controller

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Task"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
)

// ProjectTags handles GET /project/{project_uuid}/tags: the tags in use on
// the project's tasks, most used first, for the tag picker and the board's
// tag filter. Members may read them.
func ProjectTags(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectaccess.Require(w, r, false, "")
	if !ok {
		return
	}
	tags, err := business.ProjectTags(r.Context(), projectID.String())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectTags err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't load the project's tags. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": tags})
}
