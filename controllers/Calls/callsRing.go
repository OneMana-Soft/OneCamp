package controllers

// POST /calls/{session_id}/ring (router/callsSessions.go): someone in a DM's
// or group's call rings its people again (slack-06 H2), from the call's
// "Ring again". Call State decides who may; business/Chat rings whoever isn't
// in the call, a decliner too, and tells the asker whom.
//   - 200 {session, rung}: rung again (0 when everyone is in already).
//   - 404: no such call going on, not a DM's or a group's, or one in a
//     conversation the asker isn't in (the words GetCall answers).
//   - 409 not_in_call: the asker isn't in the call.
//   - 429 still_ringing: its last ring still rings; 429 ring_limit: it was
//     rung again RingAgainMax times.

import (
	"errors"
	"net/http"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// RingCall POST /calls/{session_id}/ring — someone in a DM's or group's call
// rings its people again.
func RingCall(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := signedIn(w, r)
	if !ok {
		return
	}
	s, err := callStateBusiness.MayRingAgain(r.Context(), &userInfo, chi.URLParam(r, "session_id"))
	switch {
	case errors.Is(err, callStateBusiness.ErrNothingToDecline):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no call here you can see."})
		return
	case errors.Is(err, callStateBusiness.ErrRingNotInCall):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_in_call", "msg": "Join the call to ring its people."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/RingCall failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	in := make([]string, 0, len(s.People))
	for _, p := range s.InCall() {
		if p.UserUUID != "" {
			in = append(in, p.UserUUID)
		}
	}
	call := chatBusiness.RingingCall{Session: s.ID, Kind: s.Resource.Kind, Resource: s.Resource.ID, StartedAt: s.StartedAt, InCall: in}
	n, err := chatBusiness.RingAgain(r.Context(), call, userInfo.UserPostgresInfo.Id.String(), time.Now())
	switch {
	case errors.Is(err, chatBusiness.ErrStillRinging):
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"code": "still_ringing", "msg": "It's still ringing."})
		return
	case errors.Is(err, chatBusiness.ErrRingLimit):
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"code": "ring_limit", "msg": "You've rung them again as often as a call can."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calls/RingCall failed: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"session": s.ID, "rung": n}})
}
