// Package projectaccess answers one question for project-scoped endpoints:
// may the caller see (or change) the project in the URL? Members see; the
// project's admins, who also edit its tasks, change. It writes the refusal
// itself, so a handler just returns when ok is false.
package projectaccess

import (
	"net/http"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Require reads {project_uuid} and checks the caller's place in it. A project
// they can't see answers as one that doesn't exist. deniedMsg is what an
// admin-only refusal says.
func Require(w http.ResponseWriter, r *http.Request, needAdmin bool, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, bool) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	raw := chi.URLParam(r, "project_uuid")
	id, err := uuid.Parse(raw)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return uuid.Nil, nil, false
	}
	p, err := projectBusiness.GetBasicDgraphProjectInfo(r.Context(), raw, userInfo.UserDgraphInfo.Uid)
	if err != nil || p == nil || (p.IsProjectMember == 0 && p.IsProjectAdmin == 0) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Project not found"})
		return uuid.Nil, nil, false
	}
	if needAdmin && p.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": deniedMsg})
		return uuid.Nil, nil, false
	}
	return id, p, true
}
