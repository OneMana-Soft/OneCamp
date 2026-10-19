package Calendar

// Reminders over HTTP (business/Calendar reminders.go, calendar A5): a
// person's own reminders for an event, and a reminder snoozed. Signed-in
// members only (router/calendarReminders.go).

import (
	"encoding/json"
	"io"
	"net/http"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// reminderBodyLimit is the most a reminders write carries.
const reminderBodyLimit = 2 << 10

// GetCalendarReminders is the reader's own reminders for an event or series.
// GET /calendar/events/{eventId}/reminders
func GetCalendarReminders(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	rem, err := business.Events.Reminders(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeCalendarError(w, r, "GetCalendarReminders", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rem})
}

// SetCalendarReminders sets them: minutes before, up to five; null goes back
// to the default and [] is none.
// PUT /calendar/events/{eventId}/reminders {minutes}
func SetCalendarReminders(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var in struct {
		Minutes []int `json:"minutes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, reminderBodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read those reminders."})
		return
	}
	rem, err := business.Events.SetReminders(r.Context(), who, chi.URLParam(r, "eventId"), in.Minutes)
	if err != nil {
		writeCalendarError(w, r, "SetCalendarReminders", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rem})
}

// SnoozeCalendarReminder brings a reminder of an occurrence back: in five
// minutes ("5m"), when it starts ("start"), or a minute before ("1m").
// POST /calendar/events/{eventId}/snooze {until}
func SnoozeCalendarReminder(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var in struct {
		Until string `json:"until"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, reminderBodyLimit)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that snooze."})
		return
	}
	at, err := business.Events.Snooze(r.Context(), who, chi.URLParam(r, "eventId"), business.SnoozeFor(in.Until))
	if err != nil {
		writeCalendarError(w, r, "SnoozeCalendarReminder", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"at": at.UTC().Format("2006-01-02T15:04:05Z07:00")}})
}
