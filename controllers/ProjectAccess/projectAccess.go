// Package projectaccess answers one question for project-scoped endpoints:
// may the caller see (or change) the project in the URL? Members see; the
// project's admins, who also edit its tasks, change. It writes the refusal
// itself, so a handler just returns when ok is false.
package projectaccess

import (
	"errors"
	"net/http"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Require reads {project_uuid} and checks the caller's place in it. A project
// they can't see answers as one that doesn't exist; a read that failed says
// so (helpers.WriteReadFailure). deniedMsg is what an admin-only refusal says.
func Require(w http.ResponseWriter, r *http.Request, needAdmin bool, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, bool) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	raw := chi.URLParam(r, "project_uuid")
	id, err := uuid.Parse(raw)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return uuid.Nil, nil, false
	}
	p, err := projectBusiness.GetBasicDgraphProjectInfo(r.Context(), raw, userInfo.UserDgraphInfo.Uid)
	if err == nil && (p == nil || (p.IsProjectMember == 0 && p.IsProjectAdmin == 0)) {
		err = helpers.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/ProjectAccess.Require")
		return uuid.Nil, nil, false
	}
	if needAdmin && p.IsProjectAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": deniedMsg})
		return uuid.Nil, nil, false
	}
	return id, p, true
}

// RequireTask reads a task id and checks the caller may see the task. A task
// they can't see answers as one that doesn't exist; a read that failed says so.
func RequireTask(w http.ResponseWriter, r *http.Request, raw string) (uuid.UUID, *dgraphStruct.DgraphTask, bool) {
	return requireTask(w, r, raw, false)
}

// RequireTaskAdmin is RequireTask for a change to the task: the caller must
// also be one of its project's admins, as every task write requires.
func RequireTaskAdmin(w http.ResponseWriter, r *http.Request, raw string) (uuid.UUID, *dgraphStruct.DgraphTask, bool) {
	return requireTask(w, r, raw, true)
}

func requireTask(w http.ResponseWriter, r *http.Request, raw string, change bool) (uuid.UUID, *dgraphStruct.DgraphTask, bool) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	id, err := uuid.Parse(raw)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return uuid.Nil, nil, false
	}
	task, err := taskBusiness.TaskFor(r.Context(), id, userInfo.UserDgraphInfo.Uid, change)
	switch {
	case err == nil:
		return id, task, true
	case errors.Is(err, taskBusiness.ErrNotProjectAdmin):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the project's admins can change its tasks."})
	default:
		// A task they may not read is ErrTaskNotVisible, which is also
		// helpers.ErrNotFound: a 404; any other error is a failed read: a 503.
		helpers.WriteReadFailure(w, r, err, "task", "controllers/ProjectAccess.RequireTask")
	}
	return uuid.Nil, nil, false
}
