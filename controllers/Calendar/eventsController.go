package Calendar

// The calendar's own API over HTTP (business/Calendar Events, the one store;
// routes in router/calendarEvents.go, signed-in members only). Every answer is
// {"data": …} or {"msg": …}, the message written for the person: 400 for a
// request that can't be read, 403 with the reason, 404 alike for an event that
// isn't there and one that isn't theirs, 409 with the event as it is now when
// someone changed it meanwhile, 503 while the calendar's store is moving or
// isn't switched on.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// eventBodyLimit is the most an event request carries: 100,000 characters of
// notes and the rest.
const eventBodyLimit = 1 << 20

// writeCalendarError answers a call to the calendar's API that failed.
func writeCalendarError(w http.ResponseWriter, r *http.Request, where string, err error) {
	var stale *business.StaleError
	var input *business.EventInputError
	switch {
	case errors.As(err, &stale):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": stale.Error(), "data": stale.Current})
	case errors.Is(err, business.ErrMoving), errors.Is(err, business.ErrStoreOff):
		writeCalendarUnavailable(w, err)
	case errors.Is(err, business.ErrUnavailable):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That answer isn't available yet."})
	case errors.As(err, &input), errors.Is(err, business.ErrEventNotFound),
		errors.Is(err, business.ErrOnlyCreatorChanges), errors.Is(err, business.ErrNotEventCreator),
		errors.Is(err, business.ErrCreatorCannotLeave):
		writeEventError(w, err)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calendar/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

// member is the signed-in member asking.
func member(w http.ResponseWriter, r *http.Request) (*model.UserInfo, bool) {
	u, ok := r.Context().Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return nil, false
	}
	return &u, true
}

func decodeEvent(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, eventBodyLimit)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that event."})
		return false
	}
	return true
}

// queryTime reads an RFC 3339 time from the query; nil when it is absent.
func queryTime(r *http.Request, name string) (*time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// ListCalendarEvents is the asker's events in a stretch of time.
// GET /calendar/events?from=…&to=… (RFC 3339, at most 400 days apart)
func ListCalendarEvents(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	from, okFrom := queryTime(r, "from")
	to, okTo := queryTime(r, "to")
	if !okFrom || !okTo || from == nil || to == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say from when to when, as times like 2026-10-16T00:00:00Z."})
		return
	}
	events, cut, err := business.Events.Range(r.Context(), who, *from, *to)
	if err != nil {
		writeCalendarError(w, r, "ListCalendarEvents", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": events, "truncated": cut})
}

// GetCalendarEvent is one event. GET /calendar/events/{eventId}
func GetCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	e, err := business.Events.Get(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeCalendarError(w, r, "GetCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// CreateCalendarEvent makes an event on the asker's calendar.
// POST /calendar/events
func CreateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var in business.EventInput
	if !decodeEvent(w, r, &in) {
		return
	}
	e, err := business.Events.Create(r.Context(), who, in)
	if err != nil {
		writeCalendarError(w, r, "CreateCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": e})
}

// UpdateCalendarEvent changes an event at the sequence the asker read.
// PATCH /calendar/events/{eventId}
func UpdateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var p business.EventPatch
	if !decodeEvent(w, r, &p) {
		return
	}
	e, err := business.Events.Update(r.Context(), who, chi.URLParam(r, "eventId"), p)
	if err != nil {
		writeCalendarError(w, r, "UpdateCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// DeleteCalendarEvent puts an event in its organiser's trash; with
// ?sequence=, only if nobody has changed it since. For one instance of a
// repeating event, ?scope= says which: this, following (this and following)
// or all. ?notify=none is "Tell guests? Don't send".
// DELETE /calendar/events/{eventId}
func DeleteCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var sequence *int
	if v := r.URL.Query().Get("sequence"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The sequence is a number."})
			return
		}
		sequence = &n
	}
	// "Tell guests? Don't send" (?notify=none): guests are told in their inbox alone.
	ctx := business.DontSendIf(r.Context(), r.URL.Query().Get("notify"))
	if err := business.Events.Delete(ctx, who, chi.URLParam(r, "eventId"), r.URL.Query().Get("scope"), sequence); err != nil {
		writeCalendarError(w, r, "DeleteCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Moved to the trash."})
}

// RestoreCalendarEvent brings an event back from the trash.
// POST /calendar/events/{eventId}/restore
func RestoreCalendarEvent(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	e, err := business.Events.Restore(r.Context(), who, chi.URLParam(r, "eventId"))
	if err != nil {
		writeCalendarError(w, r, "RestoreCalendarEvent", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": e})
}

// CalendarTrash is the asker's deleted events, newest first, 50 at a time.
// GET /calendar/events/trash?before=… (RFC 3339: the oldest deletion seen)
func CalendarTrash(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	before, okBefore := queryTime(r, "before")
	if !okBefore {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say before when, as a time like 2026-10-16T00:00:00Z."})
		return
	}
	events, err := business.Events.Trash(r.Context(), who, before)
	if err != nil {
		writeCalendarError(w, r, "CalendarTrash", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": events})
}
