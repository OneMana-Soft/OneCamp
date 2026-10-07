package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	business "github.com/akashc777/OneCamp/business/ProjectUpdate"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Project updates: see business/ProjectUpdate. Members read a project's
// updates; its admins draft and post them; an update's author edits it, and
// the author or an admin deletes it.

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ue *business.UpdateError
	switch {
	case errors.As(err, &ue):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ue.Error()})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That update isn't there any more."})
	case errors.Is(err, business.ErrNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the person who wrote an update can change it."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectUpdate/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

func project(w http.ResponseWriter, r *http.Request, needAdmin bool) (uuid.UUID, *dgraphStruct.DgraphProject, bool) {
	return projectaccess.Require(w, r, needAdmin, "Only the project's admins can post its updates.")
}

func me(r *http.Request) *userModels.UserInfo {
	u, _ := userModels.FromContext(r.Context())
	return u
}

func readInput(w http.ResponseWriter, r *http.Request) (business.Input, bool) {
	var in business.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That update couldn't be read."})
		return in, false
	}
	return in, true
}

func updateID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "update_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That update isn't there any more."})
		return uuid.Nil, false
	}
	return id, true
}

// ListUpdates is a project's updates, newest first. GET /project/{p}/updates?limit=
// can_post tells the page whether to offer writing one.
func ListUpdates(w http.ResponseWriter, r *http.Request) {
	id, p, ok := project(w, r, false)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := business.List(r.Context(), id, limit, false)
	if err != nil {
		write(w, r, "ListUpdates", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"updates": list, "can_post": p.IsProjectAdmin > 0}})
}

// DraftUpdate drafts the next update from the project's tasks. GET /project/{p}/updates/draft?tz=
func DraftUpdate(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, true)
	if !ok {
		return
	}
	// Days are counted where the author is (?tz=Asia/Kolkata).
	d, err := business.MakeDraft(r.Context(), id, me(r).UserDgraphInfo.Uid, time.Now().In(helpers.Location(r.URL.Query().Get("tz"))))
	if err != nil {
		write(w, r, "DraftUpdate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}

// PostUpdate posts an update. POST /project/{p}/updates {health, body, shared_with_client, channel_uuid}
func PostUpdate(w http.ResponseWriter, r *http.Request) {
	id, p, ok := project(w, r, true)
	if !ok {
		return
	}
	in, ok := readInput(w, r)
	if !ok {
		return
	}
	posted, err := business.Post(r.Context(), me(r), id, p.Name, in)
	if err != nil {
		write(w, r, "PostUpdate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": posted})
}

// EditUpdate changes an update. POST /project/{p}/updates/{u}/edit
func EditUpdate(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, true)
	if !ok {
		return
	}
	uid, ok := updateID(w, r)
	if !ok {
		return
	}
	in, ok := readInput(w, r)
	if !ok {
		return
	}
	v, err := business.Edit(r.Context(), me(r), id, uid, in)
	if err != nil {
		write(w, r, "EditUpdate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// DeleteUpdate removes an update. POST /project/{p}/updates/{u}/delete
func DeleteUpdate(w http.ResponseWriter, r *http.Request) {
	id, p, ok := project(w, r, false)
	if !ok {
		return
	}
	uid, ok := updateID(w, r)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), me(r), id, uid, p.IsProjectAdmin > 0); err != nil {
		write(w, r, "DeleteUpdate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// ProjectsOverview handles GET /project/overview?tz=: every project the
// person is in, with where its tasks stand and its latest update's health.
// tz is their zone, which days are counted in.
func ProjectsOverview(w http.ResponseWriter, r *http.Request) {
	u := me(r)
	if u == nil {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return
	}
	list, err := business.Overviews(r.Context(), u.UserDgraphInfo.Uid, time.Now().In(helpers.Location(r.URL.Query().Get("tz"))))
	if err != nil {
		write(w, r, "ProjectsOverview", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"projects": list}})
}
