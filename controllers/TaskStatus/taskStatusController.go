// Package controller (TaskStatus) serves a project's task statuses: any member
// can read them; the project's admins create, change, reorder and delete the
// custom ones. See business/TaskStatus.
package controller

import (
	"encoding/json"
	"errors"
	"net/http"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	business "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type access int

const (
	noAccess access = iota
	member
	admin
)

// projectAccess reads the project from the URL and what the signed-in person
// may do in it. It writes the response and returns ok=false when they may not
// go on.
func projectAccess(w http.ResponseWriter, r *http.Request, need access) (projectID uuid.UUID, user userModels.UserInfo, ok bool) {
	user, _ = r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	projectID, err := uuid.Parse(chi.URLParam(r, "project_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That is not a project id."})
		return uuid.Nil, user, false
	}
	project, err := projectBusiness.GetBasicDgraphProjectInfo(r.Context(), projectID.String(), user.UserDgraphInfo.Uid)
	if err != nil || project == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Project not found."})
		return uuid.Nil, user, false
	}
	has := noAccess
	if project.IsProjectAdmin > 0 {
		has = admin
	} else if project.IsProjectMember > 0 {
		has = member
	} else if user.UserPostgresInfo.IsAdmin {
		// A workspace admin sets GitHub automation rules for any linked
		// project, and needs its statuses to offer them. Reading only:
		// changing them stays with the project's own admins.
		has = member
	}
	if has < need {
		msg := "Only members of this project can see its statuses."
		if need == admin {
			msg = "Only this project's admins can change its statuses."
		}
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": msg})
		return uuid.Nil, user, false
	}
	return projectID, user, true
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	// A taken name is a mistake in what was asked, like any invalid input: 409
	// reads in the app as "someone else changed this", which it is not.
	case errors.Is(err, business.ErrInvalid), errors.Is(err, business.ErrTooMany), errors.Is(err, business.ErrUnknownStatus), errors.Is(err, business.ErrNameTaken):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That status no longer exists."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskStatus err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Could not update the statuses. Try again."})
	}
}

func statusID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "status_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That is not a status id."})
		return uuid.Nil, false
	}
	return id, true
}

// List GET /project/{project_uuid}/statuses
func List(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectAccess(w, r, member)
	if !ok {
		return
	}
	res, err := business.List(r.Context(), projectID)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// Create POST /project/{project_uuid}/statuses
func Create(w http.ResponseWriter, r *http.Request) {
	projectID, user, ok := projectAccess(w, r, admin)
	if !ok {
		return
	}
	var in business.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	userID, _ := uuid.Parse(user.UserDgraphInfo.Uuid)
	s, err := business.Create(r.Context(), projectID, userID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// Update POST /project/{project_uuid}/statuses/{status_id}
func Update(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectAccess(w, r, admin)
	if !ok {
		return
	}
	id, ok := statusID(w, r)
	if !ok {
		return
	}
	var in business.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, r, business.ErrInvalid)
		return
	}
	s, err := business.Update(r.Context(), projectID, id, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// Delete POST /project/{project_uuid}/statuses/{status_id}/delete
// Body: {"move_to": "<status>"}; empty moves its tasks to the built-in status
// it counted as.
func Delete(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectAccess(w, r, admin)
	if !ok {
		return
	}
	id, ok := statusID(w, r)
	if !ok {
		return
	}
	var in struct {
		MoveTo string `json:"move_to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if err := business.Delete(r.Context(), projectID, id, in.MoveTo); err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// Reorder POST /project/{project_uuid}/statuses/reorder  Body: {"ids": [...]}
func Reorder(w http.ResponseWriter, r *http.Request) {
	projectID, _, ok := projectAccess(w, r, admin)
	if !ok {
		return
	}
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.IDs) > business.MaxPerProject {
		fail(w, r, business.ErrInvalid)
		return
	}
	if err := business.Reorder(r.Context(), projectID, in.IDs); err != nil {
		fail(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Saved"})
}
