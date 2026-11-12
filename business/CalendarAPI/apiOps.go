// Package business (CalendarAPI) is the calendar on the public API (gcal-15
// head A): its calendars and events, and when people are free, each declared
// once as an op (business/PublicAPI), so it is a /v1 route and, in the AI
// edition, a governed MCP tool (business/MCPServer PublishOps) of the same
// name: list_events, get_event, find_time, create_event, update_event,
// cancel_event and restore_event, and list_calendars.
//
// Permission model: every op acts as the token's member (the token check put
// them on the context, after BoundAgent; a token never lends an admin's
// powers, and the calendar asks none) through the calendar's own service
// (business/Calendar Events, the one /calendar/events answers with, so the
// same JSON) and the Availability Engine's suggestions (/event/suggestTimes).
// Reads need calendar:read and follow the calendar's sharing levels and
// visibility: someone who sees a calendar only as busy is answered Busy
// blocks, and a private event is not found by anyone not on it. Writes need
// calendar:write and are made only where the member may (the calendar's
// write rule, the event's own change rule); a delete goes to the trash, and
// restore_event brings it back. An event's read says where a member joins its
// call room (room.join, gcal-15 head B: the app's page, which asks for a token
// by the calendar's own rule when someone joins; no secret here); webhooks are
// unchanged, ids and times only.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// Events are the webhook kinds the calendar sends (business/Calendar
// webhooks.go), for the reference.
var Events = []string{"event.created", "event.updated", "event.cancelled", "event.deleted", "event.restored"}

// noApps is why an app is refused an op that acts on no place: an app has no
// calendar of its own.
const noApps = "An app has no calendar: the calendar is a person's."

// The arguments an event is written with: /calendar/events' own (an
// EventInput, and an EventPatch's sequence), so a body that works there works
// here. Its times, guests, repeat rule and files are the calendar's to check.
const eventFields = `"kind":{"type":"string","maxLength":32},"title":{"type":"string","maxLength":1000},` +
	`"notes":{"type":"string","maxLength":100000},"notes_format":{"type":"string","maxLength":16},"location":{"type":"string","maxLength":1000},` +
	`"start":{"description":"{at: RFC 3339 time, zone} or {date: YYYY-MM-DD} for all day"},"end":{"description":"as start"},` +
	`"transparency":{"type":"string","enum":["opaque","transparent"]},"visibility":{"type":"string","maxLength":16},"hue":{"type":"string","maxLength":32},` +
	`"attendees":{"description":"guests: [{person_id} or {email}]"},"guests_can_modify":{"type":"boolean"},"guests_can_invite":{"type":"boolean"},` +
	`"guests_can_see_guests":{"type":"boolean"},"recurrence":{"description":"a repeat rule: {rrule}"},"sync_to_google":{"type":"boolean"},` +
	`"calendar_id":{"type":"string","maxLength":64},"notify":{"type":"string","enum":["","none"]},"email_on_answers":{"type":"boolean"},` +
	`"task_uuid":{"type":"string","maxLength":64},"out_of_office":{"description":"an out-of-office's decline rule"},"files":{"description":"docs, boards and tasks on it: [{kind: doc, board or task, uuid}]"}`

const idField = `"id":{"type":"string","maxLength":128,"description":"The event's id (an instance of a repeating one too)."}`

// APIOps are the calendar's ops, registered as the calendar resource's routes
// (controllers/V1).
func APIOps() []v1api.Op {
	read, write := apiTokenBusiness.ScopeCalendarRead, apiTokenBusiness.ScopeCalendarWrite
	reads := v1api.Behaviour{ReadOnly: true, Idempotent: true}
	return []v1api.Op{
		{Name: "list_calendars", Summary: "The calendars you may see: your own, then each project's and those shared with you",
			Method: http.MethodGet, Path: "/calendars", Scope: read, Behaviour: reads, Unplaced: "workspace", AppsRefused: noApps,
			Input: json.RawMessage(`{"type":"object","properties":{}}`), Serve: serveCalendars},
		{Name: "list_events", Summary: "Events between two times: yours, or on one calendar you may see (busy-only calendars answer Busy blocks); room.join is where a member joins one's call",
			Method: http.MethodGet, Path: "/events", Scope: read, Behaviour: reads, Unplaced: "workspace", AppsRefused: noApps,
			Input: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","maxLength":40,"description":"RFC 3339"},` +
				`"to":{"type":"string","maxLength":40,"description":"RFC 3339, at most 400 days after from"},"calendar_id":{"type":"string","maxLength":64},` +
				`"cursor":{"type":"string","maxLength":16},"limit":{"type":"integer","minimum":1,"maximum":200}},"required":["from","to"]}`),
			Serve: serveEvents},
		{Name: "get_event", Summary: "One event you are on, as the calendar shows it to you; room.join is where a member joins its call",
			Method: http.MethodGet, Path: "/events/{id}", Scope: read, Behaviour: reads, Reach: eventReach,
			Input: json.RawMessage(`{"type":"object","properties":{` + idField + `},"required":["id"]}`), Serve: serveEvent},
		{Name: "find_time", Summary: "Times you and the people named are free, in each one's working hours, best first",
			Method: http.MethodPost, Path: "/events/find-time", Scope: read, Behaviour: reads, Unplaced: "workspace", AppsRefused: noApps,
			Input: json.RawMessage(`{"type":"object","properties":{"participants":{"type":"array","maxItems":20,"items":{"type":"string","maxLength":64}},` +
				`"duration_minutes":{"type":"integer","minimum":` + strconv.Itoa(availabilityBusiness.MinMeetingMinutes) +
				`,"maximum":` + strconv.Itoa(availabilityBusiness.MaxMeetingMinutes) + `},"from":{"type":"string","maxLength":40,"description":"an RFC 3339 time, before to"},` +
				`"to":{"type":"string","maxLength":40,"description":"an RFC 3339 time"},"hours":{"description":"working hours to search in"}},` +
				`"required":["duration_minutes","from","to"]}`),
			Serve: serveFindTime},
		{Name: "create_event", Summary: "Make an event on your calendar, or on a calendar you may edit",
			Method: http.MethodPost, Path: "/events", Scope: write, Behaviour: v1api.Behaviour{}, Unplaced: "self", AppsRefused: noApps,
			Input: json.RawMessage(`{"type":"object","properties":{` + eventFields + `},"required":["start","end"]}`), Serve: serveCreate},
		{Name: "update_event", Summary: "Change an event you may change; send the sequence you read, so nobody's newer change is lost",
			Method: http.MethodPatch, Path: "/events/{id}", Scope: write, Behaviour: v1api.Behaviour{Idempotent: true}, Reach: eventReach,
			Input: json.RawMessage(`{"type":"object","properties":{` + idField + `,"sequence":{"type":"integer","minimum":0},` + eventFields + `},"required":["id"]}`),
			Serve: serveUpdate},
		{Name: "cancel_event", Summary: "Move an event to the trash (one instance, this and following, or all of a series); restore_event brings it back",
			Method: http.MethodDelete, Path: "/events/{id}", Scope: write, Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
			UndoneBy: []string{"restore_event"}, Reach: eventReach,
			Input: json.RawMessage(`{"type":"object","properties":{` + idField + `,"scope":{"type":"string","enum":["this","following","all"]},` +
				`"sequence":{"type":"integer","minimum":0},"notify":{"type":"string","enum":["","none"]}},"required":["id"]}`),
			Serve: serveCancel},
		{Name: "restore_event", Summary: "Bring an event back from the trash",
			Method: http.MethodPost, Path: "/events/{id}/restore", Scope: write, Behaviour: v1api.Behaviour{Idempotent: true}, Reach: eventReach,
			Input: json.RawMessage(`{"type":"object","properties":{` + idField + `},"required":["id"]}`), Serve: serveRestore},
	}
}

// eventReach is the event an op's "id" names: an app is held to it (and has
// none), MCP checks it by the event's own rule.
func eventReach(args map[string]any) (string, string) {
	id, _ := args["id"].(string)
	return calendarBusiness.KindEvent, id
}

// member is the token's member, as the token check put them on ctx.
func member(ctx context.Context) *model.UserInfo {
	who, _ := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	return &who
}

// decode reads args into what the calendar takes, as /calendar/events reads
// its body, leaving out the arguments that aren't the body's.
func decode(args map[string]any, into any, drop ...string) error {
	body := make(map[string]any, len(args))
	for k, v := range args {
		body[k] = v
	}
	for _, k := range drop {
		delete(body, k)
	}
	raw, err := json.Marshal(body)
	if err == nil {
		err = json.Unmarshal(raw, into)
	}
	if err != nil {
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "the event's fields are in the reference, GET /v1/openapi.json", "couldn't read that event")
	}
	return nil
}

func serveCalendars(ctx context.Context, _ v1api.Call) (any, error) {
	cals, err := calendarBusiness.Events.Calendars(ctx, member(ctx))
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return v1api.Page[calendarBusiness.CalendarView]{Items: cals}, nil
}

// EventPage is a page of events; Truncated says the stretch held more than
// the calendar answers at once (ask a shorter one).
type EventPage struct {
	Items      []calendarBusiness.Event `json:"items"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Truncated  bool                     `json:"truncated,omitempty"`
}

func serveEvents(ctx context.Context, call v1api.Call) (any, error) {
	from, errFrom := time.Parse(time.RFC3339Nano, strings.TrimSpace(argString(call.Args, "from")))
	to, errTo := time.Parse(time.RFC3339Nano, strings.TrimSpace(argString(call.Args, "to")))
	if errFrom != nil || errTo != nil {
		return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "send times like 2026-10-16T00:00:00Z", "say from when to when")
	}
	who := member(ctx)
	var events []calendarBusiness.Event
	var cut bool
	var err error
	if cal := strings.TrimSpace(argString(call.Args, "calendar_id")); cal != "" {
		events, cut, err = calendarBusiness.Events.CalendarEvents(ctx, who, []string{cal}, from, to)
	} else {
		events, cut, err = calendarBusiness.Events.Range(ctx, who, from, to)
	}
	if err != nil {
		return nil, apiError(ctx, err)
	}
	cursor, limit := v1api.PageArgs(call.Args)
	at := 0
	if cursor != "" {
		if at, err = strconv.Atoi(cursor); err != nil || at < 0 {
			return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "send the next_cursor a page gave", "that cursor isn't one")
		}
	}
	page := EventPage{Items: []calendarBusiness.Event{}, Truncated: cut}
	if at < len(events) {
		end := min(len(events), at+limit)
		page.Items = events[at:end]
		if end < len(events) {
			page.NextCursor = strconv.Itoa(end)
		}
	}
	return page, nil
}

func argString(args map[string]any, name string) string {
	s, _ := args[name].(string)
	return s
}

func serveEvent(ctx context.Context, call v1api.Call) (any, error) {
	e, err := calendarBusiness.Events.Get(ctx, member(ctx), argString(call.Args, "id"))
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return e, nil
}

func serveFindTime(ctx context.Context, call v1api.Call) (any, error) {
	var in calendarBusiness.FindTimeInput
	if err := decode(call.Args, &in); err != nil {
		return nil, err
	}
	res, err := availabilityBusiness.Suggestions(ctx, member(ctx).UserDgraphInfo.Uuid, in, time.Now())
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return res, nil
}

func serveCreate(ctx context.Context, call v1api.Call) (any, error) {
	var in calendarBusiness.EventInput
	if err := decode(call.Args, &in); err != nil {
		return nil, err
	}
	e, err := calendarBusiness.Events.Create(ctx, member(ctx), in)
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return e, nil
}

func serveUpdate(ctx context.Context, call v1api.Call) (any, error) {
	var p calendarBusiness.EventPatch
	if err := decode(call.Args, &p, "id"); err != nil {
		return nil, err
	}
	e, err := calendarBusiness.Events.Update(ctx, member(ctx), argString(call.Args, "id"), p)
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return e, nil
}

func serveCancel(ctx context.Context, call v1api.Call) (any, error) {
	var opts struct {
		Sequence *int `json:"sequence"`
	}
	if err := decode(call.Args, &opts, "id", "scope", "notify"); err != nil {
		return nil, err
	}
	id := argString(call.Args, "id")
	ctx = calendarBusiness.DontSendIf(ctx, argString(call.Args, "notify"))
	if err := calendarBusiness.Events.Delete(ctx, member(ctx), id, argString(call.Args, "scope"), opts.Sequence); err != nil {
		return nil, apiError(ctx, err)
	}
	return map[string]any{"id": id, "deleted": true}, nil
}

func serveRestore(ctx context.Context, call v1api.Call) (any, error) {
	e, err := calendarBusiness.Events.Restore(ctx, member(ctx), argString(call.Args, "id"))
	if err != nil {
		return nil, apiError(ctx, err)
	}
	return e, nil
}

// apiError is a calendar refusal in the API's one error body, with the
// statuses /calendar/events answers (controllers/Calendar writeEventError):
// an event that isn't there and one kept from the caller alike are 404.
func apiError(ctx context.Context, err error) error {
	var stale *calendarBusiness.StaleError
	var input *calendarBusiness.EventInputError
	var refused *calendarBusiness.EventRefusal
	var readOnly *calendarBusiness.CalendarReadOnlyError
	var refusal *availabilityBusiness.Refusal
	switch {
	case errors.As(err, &stale):
		return v1api.Errorf(http.StatusConflict, "conflict", "read it again and send its new sequence", "%s", stale.Error())
	case errors.As(err, &readOnly):
		return v1api.Errorf(http.StatusConflict, "calendar_read_only", "put it on another calendar", "%s", readOnly.Error())
	case errors.Is(err, calendarBusiness.ErrMoving), errors.Is(err, calendarBusiness.ErrStoreOff):
		return v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "try again later", "%s", err.Error())
	case errors.Is(err, calendarBusiness.ErrEventNotFound):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", "event not found or you don't have access")
	case errors.Is(err, calendarBusiness.ErrOnlyCreatorChanges), errors.Is(err, calendarBusiness.ErrNotEventCreator),
		errors.Is(err, calendarBusiness.ErrCreatorCannotLeave), errors.As(err, &refused):
		return v1api.Errorf(http.StatusForbidden, "forbidden", "", "%s", err.Error())
	case errors.As(err, &input), errors.Is(err, calendarBusiness.ErrUnavailable):
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s", err.Error())
	case errors.As(err, &refusal):
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s", refusal.Msg)
	}
	helpers.LogErrorWithContext(ctx, "business/CalendarAPI: %+v", err)
	return v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "try again in a moment", "the calendar couldn't answer")
}
