package controllers

// POST /calls/{session_id}/decline (router/callsSessions.go): a member of a
// DM or group says no to its call ringing them (slack-06 H2). Call State
// decides who may and records it once; business/Chat stops the decliner's
// ring on every device and tells the call's starter who said no.
//   - 200 {session, declined: true}: declined, now or before (a repeat keeps
//     one row and tells nobody again, unless it answers a ring again).
//   - 404: no such call going on, not a DM's or a group's, or one in a
//     conversation the asker isn't in (the words GetCall answers).
//   - 409 in_call: the asker is in the call.

import (
	"errors"
	"net/http"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/go-chi/chi/v5"
)

// DeclineCall POST /calls/{session_id}/decline — a member of a DM or group
// says no to its call ringing them.
func DeclineCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	s, first, err := callStateBusiness.Decline(r.Context(), &userInfo, chi.URLParam(r, "session_id"))
	switch {
	case errors.Is(err, callStateBusiness.ErrNothingToDecline):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no call here you can see."})
		return
	case errors.Is(err, callStateBusiness.ErrDeclineInCall):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "in_call", "msg": "You're in this call. Leave it instead."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/DeclineCall failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	// A first decline is told; a repeat is told only when it answers a ring
	// again that rang them since (asked either way, so it answers once).
	again := chatBusiness.DeclinesRingAgain(s.ID, userInfo.UserPostgresInfo.Id.String())
	if first || again {
		chatBusiness.CallDeclined(s.ID, s.StartedBy, mqttStruct.CallRingPerson{UUID: userInfo.UserPostgresInfo.Id.String(), Name: userInfo.UserDgraphInfo.DisplayName()})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"session": s.ID, "declined": true}})
}
