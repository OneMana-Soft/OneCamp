package Calendar

// Answers and invitations over HTTP (business/Calendar attendance.go, calendar
// A2): a member answers an event they are on; its organiser asks again those
// who haven't answered, and copies the answer link of a guest invited by
// email. Signed-in members only (router/calendarAnswers.go).

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// answerBodyLimit is the most an answer carries: a 500-character note and the rest.
const answerBodyLimit = 8 << 10

// AnswerCalendarEvent is a guest's yes, no or maybe, with a note.
// POST /calendar/events/{eventId}/answer {answer, note, scope}
func AnswerCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var in business.AnswerInput
	if err := json.NewDecoder(io.LimitReader(r.Body, answerBodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that answer."})
		return
	}
	e, err := business.Events.Answer(r.Context(), who, chi.URLParam(r, "eventId"), in)
	if err != nil {
		writeCalendarError(w, r, "AnswerCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// NudgeCalendarGuests asks again every guest who hasn't answered.
// POST /calendar/events/{eventId}/nudge
func NudgeCalendarGuests(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	n, err := business.Events.Nudge(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeCalendarError(w, r, "NudgeCalendarGuests", err)
		return
	}
	msg := "Nobody else to ask: everyone has answered, or was asked in the last day."
	switch {
	case n == 1:
		msg = "Asked 1 guest again."
	case n > 1:
		msg = "Asked " + strconv.Itoa(n) + " guests again."
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]int{"asked": n}, "msg": msg})
}

// CalendarGuestLink is the answer link of a guest invited by email, for its
// organiser to share. POST /calendar/events/{eventId}/guest-link {email}
func CalendarGuestLink(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, answerBodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	link, err := business.Events.GuestLink(r.Context(), who, chi.URLParam(r, "eventId"), in.Email)
	if err != nil {
		writeCalendarError(w, r, "CalendarGuestLink", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"url": link}})
}
