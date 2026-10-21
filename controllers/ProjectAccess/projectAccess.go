// Package projectaccess is Work Access for HTTP handlers: may the caller do
// this with the project or the task in the request? The rule is
// business/WorkAccess's; this writes its answer. Someone a project isn't
// there for (outside it, and not stewarding it for this) finds nothing, as if
// it didn't exist; someone who may see it but not do this is told why in
// words (403); a read that failed is a 503. A handler just returns when ok is
// false.
package projectaccess

import (
	"errors"
	"net/http"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Require reads {project_uuid} and checks the caller may do need there.
// deniedMsg, when not empty, is what a refusal says instead of the rule's
// words.
func Require(w http.ResponseWriter, r *http.Request, need workAccess.Need, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, bool) {
	id, p, _, ok := RequireStanding(w, r, need, deniedMsg)
	return id, p, ok
}

// RequireStanding is Require with the caller's standing in the project, for
// a handler that says what they may do there (a list's can_edit).
func RequireStanding(w http.ResponseWriter, r *http.Request, need workAccess.Need, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, workAccess.Standing, bool) {
	return requireID(w, r, chi.URLParam(r, "project_uuid"), need, deniedMsg)
}

// RequireID is Require for a project named by raw (a body's project_uuid).
func RequireID(w http.ResponseWriter, r *http.Request, raw string, need workAccess.Need, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, bool) {
	id, p, _, ok := requireID(w, r, raw, need, deniedMsg)
	return id, p, ok
}

func requireID(w http.ResponseWriter, r *http.Request, raw string, need workAccess.Need, deniedMsg string) (uuid.UUID, *dgraphStruct.DgraphProject, workAccess.Standing, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return uuid.Nil, nil, workAccess.Standing{}, false
	}
	p, err := projectBusiness.GetBasicDgraphProjectInfo(r.Context(), id.String(), callerOf(r).UserDgraphInfo.Uid)
	if err == nil && (p == nil || p.Uuid == "") {
		err = helpers.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/ProjectAccess.Require")
		return uuid.Nil, nil, workAccess.Standing{}, false
	}
	s, ok := Allow(w, r, p, need, deniedMsg)
	if !ok {
		return uuid.Nil, nil, s, false
	}
	return id, p, s, true
}

// Allow checks a project the handler has read for the caller (its member,
// admin and team admin counts theirs) and writes the refusal when they may
// not do need. Their standing is answered, for what the handler shows.
func Allow(w http.ResponseWriter, r *http.Request, p *dgraphStruct.DgraphProject, need workAccess.Need, deniedMsg string) (workAccess.Standing, bool) {
	s, err := workAccess.Of(r.Context(), callerOf(r).UserDgraphInfo.Uuid, p)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectAccess.Allow couldn't read the caller's role: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't check what you can do in this project.")
		return s, false
	}
	stewarding := s.Steward && (need == workAccess.NeedManagePeople || need == workAccess.NeedArchive)
	if p == nil || (!s.Can(workAccess.NeedView) && !stewarding) {
		helpers.WriteReadFailure(w, r, helpers.ErrNotFound, "project", "controllers/ProjectAccess.Allow")
		return s, false
	}
	if err := s.Check(need); err != nil {
		refuse(w, err, deniedMsg)
		return s, false
	}
	return s, true
}

// RequireTask reads a task by id, or by its key ("Q4-12"), and checks the
// caller may do need with it. A key that names no task, or one they can't
// open, answers as a task that isn't there.
func RequireTask(w http.ResponseWriter, r *http.Request, raw string, need workAccess.Need) (uuid.UUID, *dgraphStruct.DgraphTask, bool) {
	id, err := taskKeys.Ref(r.Context(), raw)
	if errors.Is(err, taskKeys.ErrNoTask) {
		if _, _, isKey := taskKeys.Parse(raw); isKey {
			WriteTaskRefusal(w, r, taskBusiness.ErrTaskNotVisible)
			return uuid.Nil, nil, false
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a task."})
		return uuid.Nil, nil, false
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectAccess.RequireTask couldn't look the key up: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't look that task up.")
		return uuid.Nil, nil, false
	}
	caller := callerOf(r)
	task, _, err := taskBusiness.TaskFor(r.Context(), id, &caller, need)
	if err != nil {
		WriteTaskRefusal(w, r, err)
		return uuid.Nil, nil, false
	}
	return id, task, true
}

// AllowTask checks a task the handler has read for the caller (its project's
// counts theirs, with its assignee and maker) and writes the refusal when
// they may not do need with it.
func AllowTask(w http.ResponseWriter, r *http.Request, t *dgraphStruct.DgraphTask, need workAccess.Need) bool {
	caller := callerOf(r)
	if t == nil || t.Uuid == "" || !taskBusiness.CanViewTask(t, caller.UserDgraphInfo.Uid) {
		WriteTaskRefusal(w, r, taskBusiness.ErrTaskNotVisible)
		return false
	}
	if need == workAccess.NeedView {
		return true
	}
	s, f, err := workAccess.OfTask(r.Context(), caller.UserDgraphInfo.Uuid, caller.UserDgraphInfo.Uid, t)
	if err == nil {
		err = s.CheckTask(need, f)
	}
	if err != nil {
		WriteTaskRefusal(w, r, err)
		return false
	}
	return true
}

// WriteTaskRefusal answers a task check that said no: a task they may not
// read as one that isn't there (404), a refusal in its words (403), anything
// else as a failed read (503).
func WriteTaskRefusal(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, workAccess.ErrNotAllowed) {
		refuse(w, err, "")
		return
	}
	// A task they may not read is ErrTaskNotVisible, which is also
	// helpers.ErrNotFound: a 404; any other error is a failed read: a 503.
	helpers.WriteReadFailure(w, r, err, "task", "controllers/ProjectAccess.RequireTask")
}

// refuse writes a refusal: 403, in the rule's words or deniedMsg.
func refuse(w http.ResponseWriter, err error, deniedMsg string) {
	msg := deniedMsg
	if msg == "" {
		msg = err.Error()
	}
	helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": msg})
}

// callerOf is the signed-in member the auth middleware put on the request.
func callerOf(r *http.Request) userModels.UserInfo {
	u, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return u
}
