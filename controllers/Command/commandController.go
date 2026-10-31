// Package controllers (Command) exposes the user-facing slash command HTTP
// surface: catalog (for the composer typeahead), execute, and the old
// interact route, which only says a card is out of date (presses go to
// POST /cards/act).
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

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

// GiphyPicker handles GET /command/giphy?q=&offset= — a page of the
// composer's GIF picker: trending with no q, else a search. Giphy failing
// answers 503, which Cloudflare passes on as itself (a 502 reaches the app as
// a network error).
func GiphyPicker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "offset must be a whole number"})
			return
		}
		offset = n
	}

	page, err := business.GiphyPicker(ctx, strings.TrimSpace(r.URL.Query().Get("q")), offset)
	switch {
	case errors.Is(err, business.ErrGiphyBadRequest):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Giphy can serve a search of up to 50 characters, from offset 0 to 4999"})
	case errors.Is(err, business.ErrGiphyUnavailable):
		helpers.LogErrorWithContext(ctx, "controllers/Command/GiphyPicker err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Giphy didn't answer. Try again in a moment."})
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/Command/GiphyPicker err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load GIFs"})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
	}
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

// Interact is POST /command/interact, kept for a tab opened before cards were
// sealed: it runs nothing, and answers that the card is out of date and how to
// get a fresh one. Every press goes to POST /cards/act.
func Interact(w http.ResponseWriter, r *http.Request) {
	var req commandAdapter.InteractRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.HandleInteract(req.Command)})
}
