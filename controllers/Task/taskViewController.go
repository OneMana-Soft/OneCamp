package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Saved task views: a person's named filters, sort and columns for a
// project's task list or for My Tasks. Private to them, so the scope needs no
// membership check: it only says which list a view belongs to.

const (
	maxViewsPerScope = 30
	maxViewName      = 60
	maxViewState     = 8 << 10
)

// checkTaskView validates a view to save and returns its tidied name. Pure.
func checkTaskView(scope, name string, state json.RawMessage) (string, error) {
	if scope != "mine" {
		id, ok := strings.CutPrefix(scope, "project:")
		if _, err := uuid.Parse(id); !ok || err != nil {
			return "", errors.New("That isn't a task list.")
		}
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("Give the view a name.")
	}
	if utf8.RuneCountInString(name) > maxViewName {
		return "", errors.New("Keep the name under 60 characters.")
	}
	var obj map[string]json.RawMessage
	if len(state) > maxViewState || json.Unmarshal(state, &obj) != nil {
		return "", errors.New("That view can't be saved.")
	}
	return name, nil
}

// GetTaskViews lists the person's views for a list.
// GET /task/views?scope=mine|project:<uuid>
func GetTaskViews(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	scope := r.URL.Query().Get("scope")
	if _, err := checkTaskView(scope, "x", json.RawMessage(`{}`)); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	views, err := taskViewModel.List(userInfo.UserPostgresInfo.Id, scope)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetTaskViews err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load your views. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": views})
}

type taskViewInput struct {
	Scope string          `json:"scope"`
	Name  string          `json:"name"`
	State json.RawMessage `json:"state"`
}

// SaveTaskView saves a view; the same name in the same list replaces it.
// POST /task/views {scope, name, state}
func SaveTaskView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	userID := userInfo.UserPostgresInfo.Id
	var in taskViewInput
	if err := json.NewDecoder(io.LimitReader(r.Body, maxViewState+1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	name, err := checkTaskView(in.Scope, in.Name, in.State)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	// Replacing a view never counts against the limit; a new one might.
	exists, err := taskViewModel.Exists(userID, in.Scope, name)
	if err == nil && !exists {
		var n int
		if n, err = taskViewModel.Count(userID, in.Scope); err == nil && n >= maxViewsPerScope {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "You have 30 views here. Delete one to save another."})
			return
		}
	}
	var view *taskViewModel.TaskView
	if err == nil {
		view, err = taskViewModel.Save(userID, in.Scope, name, in.State)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SaveTaskView err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't save the view. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// DeleteTaskView deletes one of the person's views.
// POST /task/views/delete {id}
func DeleteTaskView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in struct {
		Id string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	id, err := uuid.Parse(in.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a view."})
		return
	}
	ok, err := taskViewModel.Delete(userInfo.UserPostgresInfo.Id, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DeleteTaskView err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't delete the view. Try again."})
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "View not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}
