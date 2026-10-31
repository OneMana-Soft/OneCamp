package Calendar

import (
	"errors"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Calendar"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// AddCalendarEventCall gives an event its own call (calendar B11), or answers
// the one it has: where its guests come in. For whoever may change the event.
// POST /calendar/events/{eventId}/call
func AddCalendarEventCall(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	call, err := business.Events.AddCall(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeEventCallError(w, r, "AddCalendarEventCall", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": call})
}

// RemoveCalendarEventCall takes an event's call off and ends it. For whoever
// may change the event. DELETE /calendar/events/{eventId}/call
func RemoveCalendarEventCall(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	if err := business.Events.RemoveCall(r.Context(), who, chi.URLParam(r, "eventId")); err != nil {
		writeEventCallError(w, r, "RemoveCalendarEventCall", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "The call was taken off this event."})
}

// writeEventCallError answers an event call's refusals: the workspace's
// switches in words, then as any calendar error.
func writeEventCallError(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "guests_off", "msg": "Guest access is off for this workspace, so events can't have calls of their own."})
	case errors.Is(err, guestBusiness.ErrRoomsOff):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "rooms_off", "msg": "Meeting rooms are off for this workspace, so events can't have calls of their own."})
	case errors.Is(err, business.ErrNoEventCalls):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": err.Error()})
	default:
		writeCalendarError(w, r, where, err)
	}
}
