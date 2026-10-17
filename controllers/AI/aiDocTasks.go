package controllers

// Doc → tasks (docs programme B9, ai-search-io P0-6):
//
//	POST /doc/ai/tasks         the action items in a doc, proposed as tasks
//	POST /doc/ai/tasks/create  the chosen ones made, in a project the person
//	                           may add tasks to; the answer names each task and
//	                           the line its chip goes on
//
// The rules are business/AI docTasks.go.

import (
	"encoding/json"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// FindDocActionItems handles POST /doc/ai/tasks.
func FindDocActionItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var req struct {
		DocUUID string `json:"doc_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The request couldn't be read."})
		return
	}
	res, err := business.ExtractDocActionItems(ctx, &userInfo, req.DocUUID)
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		switch {
		case errors.Is(err, ai.ErrRateLimited):
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "You've asked AI a lot in the last minute. Try again in a moment."})
		case errors.Is(err, ai.ErrCircuitOpen):
			helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
		default:
			helpers.LogErrorWithContext(ctx, "controllers/FindDocActionItems: %v", err)
			helpers.WriteRetryLater(w, "The doc couldn't be read just now.")
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": res})
}

// CreateDocActionItems handles POST /doc/ai/tasks/create.
func CreateDocActionItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var req struct {
		DocUUID     string                     `json:"doc_uuid"`
		ProjectUUID string                     `json:"project_uuid"`
		Tasks       []business.DocProposedTask `json:"tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The request couldn't be read."})
		return
	}
	res, err := business.CreateDocTasks(ctx, &userInfo, req.DocUUID, req.ProjectUUID, req.Tasks)
	if err != nil {
		if writeDocAccessError(w, err) {
			return
		}
		var cant *business.CantAddTasksError
		var invalid *business.DocAIInvalidError
		switch {
		case errors.As(err, &cant):
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": cant.Error()})
		case errors.As(err, &invalid):
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Msg})
		default:
			helpers.LogErrorWithContext(ctx, "controllers/CreateDocActionItems: %v", err)
			helpers.WriteRetryLater(w, "None of the tasks could be made just now.")
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": res})
}
