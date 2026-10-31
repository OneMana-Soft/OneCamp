// Package Preferences is Person Preferences over HTTP (design #37): the
// signed-in person reads and sets their own, and nobody else's. A token acts
// as its person, so it reads and sets that person's.
package Preferences

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// GetMine answers every preference of the signed-in person, set or default.
func GetMine(w http.ResponseWriter, r *http.Request) {
	info, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	prefs, err := preferencesBusiness.Get(r.Context(), info.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't read your preferences. Try again.", "status": "failed"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": prefs})
}

// SetMine sets one preference of the signed-in person: {"value": …}; null
// sets it back to its default.
func SetMine(w http.ResponseWriter, r *http.Request) {
	info, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2*preferencesBusiness.MaxValueBytes))
	if err != nil || json.Unmarshal(raw, &body) != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the value as {\"value\": …}.", "status": "failed"})
		return
	}
	if body.Value == nil {
		body.Value = json.RawMessage("null")
	}
	key := chi.URLParam(r, "key")
	err = preferencesBusiness.Set(r.Context(), info.UserPostgresInfo.Id, key, body.Value)
	var refused *preferencesBusiness.Refused
	switch {
	case errors.Is(err, preferencesBusiness.ErrUnknownKey):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such preference.", "status": "failed"})
	case errors.As(err, &refused):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": refused.Msg, "status": "failed"})
	case err != nil:
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't save that. Try again.", "status": "failed"})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})
	}
}
