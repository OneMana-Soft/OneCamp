package controllers

import (
	"encoding/json"
	"io"
	"net/http"

	aicoworker "github.com/akashc777/OneCamp/business/AICoworker"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// LeaveDailyNote handles POST /ai/agent-note: the app calls it when it opens,
// with the member's own calendar day, and OneCamp AI leaves today's note in
// their DM if there is one to leave. Safe to call on every open.
func LeaveDailyNote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var body struct {
		Day  string `json:"day"`
		Zone string `json:"zone"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body)
	res, err := aicoworker.LeaveDailyNote(ctx, &userInfo, body.Day, body.Zone)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/LeaveDailyNote failed: %+v", err)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// GetDailyNotePreference handles GET /ai/agent-note/preference.
func GetDailyNotePreference(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	on, err := aicoworker.NoteEnabled(ctx, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not load the setting"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]bool{"enabled": on}})
}

// SetDailyNotePreference handles POST /ai/agent-note/preference.
func SetDailyNotePreference(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := aicoworker.SetNoteEnabled(ctx, userInfo.UserPostgresInfo.Id, body.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not save the setting"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]bool{"enabled": body.Enabled}})
}
