// Package controllers (Command) exposes the user-facing slash command HTTP
// surface: catalog (for the composer typeahead), execute, and interact
// (Block Kit button/select round-trips).
package controllers

import (
	"encoding/json"
	"net/http"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	business "github.com/akashc777/OneCamp/business/Command"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// GetCatalog handles GET /command/catalog — returns the scope-filtered command
// list the composer renders when the user types "/".
func GetCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	catalog, err := business.GetCatalog(ctx, userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/GetCatalog err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load commands"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": catalog})
}

// Execute handles POST /command/execute.
func Execute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req commandAdapter.ExecuteCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	resp, err := business.Execute(ctx, userInfo, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/Execute err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Command failed"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": resp})
}

// Interact handles POST /command/interact — a button/select activation on an
// interactive card.
func Interact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req commandAdapter.InteractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	resp, err := business.HandleInteract(ctx, userInfo, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Command/Interact err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Interaction failed"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": resp})
}
