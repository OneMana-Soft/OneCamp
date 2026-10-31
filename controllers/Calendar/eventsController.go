package Calendar

// The calendar's own API over HTTP (business/Calendar Events, the one store;
// routes in router/calendarEvents.go, signed-in members only). Every answer is
// {"data": …} or {"msg": …}, the message written for the person: 400 for a
// request that can't be read, 403 with the reason, 404 alike for an event that
// isn't there and one that isn't theirs, 409 with the event as it is now when
// someone changed it meanwhile, 503 while the calendar's store is moving or
// isn't switched on.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
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
		helpers.WriteUnauthorized(w)
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

// ExportCalendar is every event the asker is on, as one iCalendar file to
// download (calendar A4): series, changed instances, guests with their
// answers, and their reminders as alarms.
// GET /calendar/events/export → 200 text/calendar
func ExportCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if _, err := business.Events.Export(r.Context(), who, &buf); err != nil {
		writeCalendarError(w, r, "ExportCalendar", err)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "OneCamp calendar.ics"}))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// GoogleSyncStatus is where the asker's move from Google Calendar stands.
// GET /calendar/sync/google → 200 {data: SyncStatus}
func GoogleSyncStatus(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	st, err := business.Events.SyncStatus(r.Context(), who)
	if err != nil {
		writeCalendarError(w, r, "GoogleSyncStatus", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": st})
}

// MoveFromGoogle starts moving every calendar of the asker's Google link
// into OneCamp (calendar B6), and answers where it stands.
// POST /calendar/sync/google/move → 202 {data: SyncStatus}
func MoveFromGoogle(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	st, err := business.Events.MoveFromGoogle(r.Context(), who)
	if err != nil {
		writeCalendarError(w, r, "MoveFromGoogle", err)
		return
	}
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"data": st})
}

// ProjectCalendar is a project's own calendar, made the first time, for a
// member who may see the project (calendar A6; the project home's Calendar
// card reads it as a "calendar" chip).
// POST /calendar/projects/{projectId}/calendar → 200 {data: {id}}
func ProjectCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	id, err := business.Events.ProjectCalendarFor(r.Context(), who, chi.URLParam(r, "projectId"))
	if err != nil {
		writeCalendarError(w, r, "ProjectCalendar", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"id": id.String()}})
}

// ListCalendars are the calendars the asker may see: their own, then each
// project's (calendar A6). GET /calendar/calendars → 200 {data: [CalendarView]}
func ListCalendars(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	cals, err := business.Events.Calendars(r.Context(), who)
	if err != nil {
		writeCalendarError(w, r, "ListCalendars", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": cals})
}

// ListEventsOnCalendars are the events on the calendars asked for (those the
// asker may see) between from and to.
// GET /calendar/calendars/events?ids=a,b&from=…&to=… → 200 {data, truncated}
func ListEventsOnCalendars(w http.ResponseWriter, r *http.Request) {
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
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" && len(ids) < 50 {
			ids = append(ids, id)
		}
	}
	events, cut, err := business.Events.CalendarEvents(r.Context(), who, ids, *from, *to)
	if err != nil {
		writeCalendarError(w, r, "ListEventsOnCalendars", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": events, "truncated": cut})
}

// PersonCalendar is a member's own calendar, to lay over the asker's: shown
// as they shared it, else as free and busy alone.
// GET /calendar/people/{userId}/calendar → 200 {data: {id}}
func PersonCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	id, err := business.Events.PersonCalendar(r.Context(), who, chi.URLParam(r, "userId"))
	if err != nil {
		writeCalendarError(w, r, "PersonCalendar", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"id": id.String()}})
}

// CalendarShares is who a calendar the asker manages is shared with.
// GET /calendar/calendars/{calendarId}/shares → 200 {data: [CalendarShare]}
func CalendarShares(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	shares, err := business.Events.Shares(r.Context(), who, chi.URLParam(r, "calendarId"))
	if err != nil {
		writeCalendarError(w, r, "CalendarShares", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": shares})
}

// ShareCalendar shares a calendar the asker manages with a member at a level,
// or ("" level) stops. PUT /calendar/calendars/{calendarId}/shares/{userId}
// {level} → 204
func ShareCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var body struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say the level: details, edit, manage, or none."})
		return
	}
	if err := business.Events.Share(r.Context(), who, chi.URLParam(r, "calendarId"), chi.URLParam(r, "userId"), body.Level); err != nil {
		writeCalendarError(w, r, "ShareCalendar", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UpdateCalendar renames or recolours a calendar the asker manages.
// PATCH /calendar/calendars/{calendarId} {name?, hue?} → 204
func UpdateCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var p business.CalendarPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<10)).Decode(&p); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say the calendar's new name or colour."})
		return
	}
	if err := business.Events.UpdateCalendar(r.Context(), who, chi.URLParam(r, "calendarId"), p); err != nil {
		writeCalendarError(w, r, "UpdateCalendar", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CalendarFeed says whether the asker has a secret feed of a calendar, and
// at what scope; its link was given once, when made (calendar B8).
// GET /calendar/calendars/{calendarId}/feed → 200 {data: FeedView}
func CalendarFeed(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	v, err := business.Events.Feed(r.Context(), who, chi.URLParam(r, "calendarId"))
	if err != nil {
		writeCalendarError(w, r, "CalendarFeed", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// MakeCalendarFeed makes the asker a new secret feed of a calendar and gives
// its link this once; a feed they had stops at once.
// PUT /calendar/calendars/{calendarId}/feed {scope: "busy"|"full"} → 200 {data: FeedView}
func MakeCalendarFeed(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var body struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose when you're busy, or everything."})
		return
	}
	v, err := business.Events.MakeFeed(r.Context(), who, chi.URLParam(r, "calendarId"), body.Scope)
	if err != nil {
		writeCalendarError(w, r, "MakeCalendarFeed", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// StopCalendarFeed stops the asker's secret feed of a calendar.
// DELETE /calendar/calendars/{calendarId}/feed → 204
func StopCalendarFeed(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	if err := business.Events.StopFeed(r.Context(), who, chi.URLParam(r, "calendarId")); err != nil {
		writeCalendarError(w, r, "StopCalendarFeed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListSubscriptions are the asker's calendars read from addresses, and how
// each was last read (calendar B8).
// GET /calendar/subscriptions → {data: [SubscriptionView]}
func ListSubscriptions(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	v, err := business.Events.Subscriptions(r.Context(), who)
	if err != nil {
		writeCalendarError(w, r, "ListSubscriptions", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// Subscribe adds a calendar read from an address. A private calendar's
// address is a secret: nothing here writes it, or the body, to a log.
// POST /calendar/subscriptions {address, name?, hue?} → 201 {data: SubscriptionView}
func Subscribe(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	var body struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Hue     string `json:"hue"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Paste the calendar's address."})
		return
	}
	v, err := business.Events.Subscribe(r.Context(), who, body.Address, body.Name, body.Hue)
	if err != nil {
		writeCalendarError(w, r, "Subscribe", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": v})
}

// RefreshSubscription reads one of the asker's subscribed calendars now.
// POST /calendar/subscriptions/{calendarId}/refresh → {data: SubscriptionView}
func RefreshSubscription(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	v, err := business.Events.RefreshSubscription(r.Context(), who, chi.URLParam(r, "calendarId"))
	if err != nil {
		writeCalendarError(w, r, "RefreshSubscription", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// Unsubscribe removes one of the asker's subscribed calendars, and
// everything on it.
// DELETE /calendar/subscriptions/{calendarId} → 204
func Unsubscribe(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	if err := business.Events.Unsubscribe(r.Context(), who, chi.URLParam(r, "calendarId")); err != nil {
		writeCalendarError(w, r, "Unsubscribe", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicCalendarFeed is the calendar a secret link names, for another
// calendar app (calendar B8): text/calendar, never kept by a cache. The link
// is a bearer secret, so nothing here writes the path, its token or its hash
// to a log; every answer that isn't the calendar is the same 404.
// GET /public/calendar/{file} (file: the token, then .ics)
func PublicCalendarFeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	body, err := business.Events.FeedICS(r.Context(), business.FeedTokenOf(chi.URLParam(r, "file")))
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	case errors.Is(err, business.ErrEventNotFound):
		helpers.WriteNotFound(w, "Not found")
	case errors.Is(err, business.ErrMoving), errors.Is(err, business.ErrStoreOff):
		helpers.WriteError(w, http.StatusServiceUnavailable, "Not available yet")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calendar/PublicCalendarFeed err: %v", err)
		helpers.WriteError(w, http.StatusServiceUnavailable, "Something went wrong")
	}
}
