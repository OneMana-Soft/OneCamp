package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	goalBusiness "github.com/akashc777/OneCamp/business/Goal"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Goals: see business/Goal. Everyone in the workspace sees the goals; a
// goal's owner, whoever made it and workspace admins change it and check in.

// bodyLimit is the most a goal or check-in request may carry: an 8,000
// character note in any script, with room for the rest.
const bodyLimit = 64 << 10

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ge *goalBusiness.GoalError
	switch {
	case errors.As(err, &ge):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ge.Error()})
	case errors.Is(err, goalBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That goal no longer exists."})
	case errors.Is(err, goalBusiness.ErrNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the goal's owner, whoever made it, or a workspace admin can change it."})
	case errors.Is(err, goalBusiness.ErrCheckInNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That check-in no longer exists."})
	case errors.Is(err, goalBusiness.ErrCheckInNotYours):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only whoever wrote the check-in can change it."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Goal/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

func userOf(r *http.Request) userModels.UserInfo {
	return r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
}

func readerOf(r *http.Request) goalBusiness.Reader {
	u := userOf(r)
	return goalBusiness.Reader{UUID: u.UserPostgresInfo.Id, DgraphUID: u.UserDgraphInfo.Uid, IsAdmin: u.UserPostgresInfo.IsAdmin}
}

// now is the time in the reader's zone (?tz=), which a goal's days are counted in.
func now(r *http.Request) time.Time { return time.Now().In(helpers.Location(r.URL.Query().Get("tz"))) }

// idParam reads a uuid from the URL, answering 404 when it isn't one.
func idParam(w http.ResponseWriter, r *http.Request, name, missing string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": missing})
		return uuid.Nil, false
	}
	return id, true
}

func goalID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	return idParam(w, r, "goal_id", "That goal no longer exists.")
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return false
	}
	return true
}

// ListGoals is every goal, open and closed. GET /goal/list?tz=
func ListGoals(w http.ResponseWriter, r *http.Request) {
	list, err := goalBusiness.List(r.Context(), readerOf(r), now(r))
	if err != nil {
		write(w, r, "ListGoals", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"goals": list}})
}

// CreateGoal makes one. POST /goal/create {title, owner_uuid, due_date, measure, …, project_uuids}
func CreateGoal(w http.ResponseWriter, r *http.Request) {
	var in goalBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	s, err := goalBusiness.Create(r.Context(), readerOf(r), in, now(r))
	if err != nil {
		write(w, r, "CreateGoal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// GetGoal is one goal with its projects, sub-goals and check-ins. GET /goal/{goal_id}?tz=
func GetGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	d, err := goalBusiness.Get(r.Context(), readerOf(r), id, now(r))
	if err != nil {
		write(w, r, "GetGoal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}

// EditGoal changes one. POST /goal/{goal_id}/edit
func EditGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	var in goalBusiness.Input
	if !decode(w, r, &in) {
		return
	}
	s, err := goalBusiness.Edit(r.Context(), readerOf(r), id, in, now(r))
	if err != nil {
		write(w, r, "EditGoal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// DeleteGoal removes one; its sub-goals move up. POST /goal/{goal_id}/delete
func DeleteGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	if err := goalBusiness.Delete(readerOf(r), id); err != nil {
		write(w, r, "DeleteGoal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Goal deleted."})
}

// ReopenGoal opens a closed goal again. POST /goal/{goal_id}/reopen
func ReopenGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	if err := goalBusiness.Reopen(readerOf(r), id); err != nil {
		write(w, r, "ReopenGoal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Goal reopened."})
}

// LinkGoalProject adds a project to a goal. POST /goal/{goal_id}/projects {project_uuid}
func LinkGoalProject(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectUUID string `json:"project_uuid"`
	}
	if !decode(w, r, &in) {
		return
	}
	pid, err := uuid.Parse(in.ProjectUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose a project to add."})
		return
	}
	if err := goalBusiness.LinkProject(r.Context(), readerOf(r), id, pid, now(r)); err != nil {
		write(w, r, "LinkGoalProject", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Project added."})
}

// UnlinkGoalProject takes a project off a goal. POST /goal/{goal_id}/projects/{project_uuid}/delete
func UnlinkGoalProject(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	pid, ok := idParam(w, r, "project_uuid", "That project isn't on this goal.")
	if !ok {
		return
	}
	if err := goalBusiness.UnlinkProject(readerOf(r), id, pid); err != nil {
		write(w, r, "UnlinkGoalProject", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Project removed."})
}

// DraftCheckIn drafts the goal's next check-in. GET /goal/{goal_id}/checkins/draft?tz=
func DraftCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	d, err := goalBusiness.MakeDraft(r.Context(), readerOf(r), id, now(r))
	if err != nil {
		write(w, r, "DraftCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": d})
}

// PostCheckIn records where the goal stands. POST /goal/{goal_id}/checkins {health, body, value, channel_uuid}
func PostCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	var in goalBusiness.CheckInInput
	if !decode(w, r, &in) {
		return
	}
	user := userOf(r)
	posted, err := goalBusiness.PostCheckIn(r.Context(), &user, readerOf(r), id, in, now(r))
	if err != nil {
		write(w, r, "PostCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": posted})
}

// EditCheckIn changes a check-in's health and note. POST /goal/{goal_id}/checkins/{checkin_id}/edit
func EditCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	cid, ok := idParam(w, r, "checkin_id", "That check-in no longer exists.")
	if !ok {
		return
	}
	var in struct {
		Health string `json:"health"`
		Body   string `json:"body"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := goalBusiness.EditCheckIn(r.Context(), readerOf(r), id, cid, in.Health, in.Body)
	if err != nil {
		write(w, r, "EditCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// DeleteCheckIn removes one. POST /goal/{goal_id}/checkins/{checkin_id}/delete
func DeleteCheckIn(w http.ResponseWriter, r *http.Request) {
	id, ok := goalID(w, r)
	if !ok {
		return
	}
	cid, ok := idParam(w, r, "checkin_id", "That check-in no longer exists.")
	if !ok {
		return
	}
	if err := goalBusiness.DeleteCheckIn(readerOf(r), id, cid); err != nil {
		write(w, r, "DeleteCheckIn", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Check-in deleted."})
}

// ProjectGoals is the open goals a project serves, for its page. GET /project/{project_uuid}/goals?tz=
func ProjectGoals(w http.ResponseWriter, r *http.Request) {
	pid, _, ok := projectaccess.Require(w, r, false, "")
	if !ok {
		return
	}
	list, err := goalBusiness.ForProject(r.Context(), readerOf(r), pid, now(r))
	if err != nil {
		write(w, r, "ProjectGoals", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"goals": list}})
}
