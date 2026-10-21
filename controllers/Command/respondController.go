package controllers

import (
	"encoding/json"
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Command"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// Respond is POST /command-responses/{token}: an app's answer to one of its
// commands, at the response URL its command was forwarded with (Slack's
// shape: text, blocks, response_type, replace_original). Public: the token is
// the credential (24 random bytes), good for five answers in 30 minutes.
func Respond(w http.ResponseWriter, r *http.Request) {
	var body business.RespondBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the answer as JSON: text, blocks, response_type."})
		return
	}
	err := business.Respond(r.Context(), chi.URLParam(r, "token"), body)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
	case errors.Is(err, business.ErrResponseGone):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": err.Error()})
	case errors.Is(err, business.ErrNoPlace):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Command/Respond: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't take that answer: " + err.Error()})
	}
}
