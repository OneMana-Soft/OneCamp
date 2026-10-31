package Integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
)

func withSyncService(t *testing.T, svc *calendar.Service) {
	t.Helper()
	prev := syncServiceFor
	syncServiceFor = func(context.Context, string) (*calendar.Service, error) { return svc, nil }
	t.Cleanup(func() { syncServiceFor = prev })
}

// Google registers as a calendar sync provider.
func TestGoogleIsACalendarSyncProvider(t *testing.T) {
	if a, ok := calendarSyncBusiness.Adapters.For("google"); !ok || a.Provider() != "google" {
		t.Fatal("Google isn't registered")
	}
}

// The calendar list: the person's own first, named by their address, then
// the others by their names, each with its zone and default reminders.
func TestGoogleCalendarsListsThePersonsOwnFirst(t *testing.T) {
	withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/users/me/calendarList") || r.URL.Query().Get("minAccessRole") != "reader" {
			t.Errorf("asked %s", r.URL)
		}
		w.Write([]byte(`{"items":[
			{"id":"team@group.calendar.google.com","summary":"Team offsites","summaryOverride":"Offsites","timeZone":"Europe/Berlin"},
			{"id":"sam@example.com","summary":"sam@example.com","primary":true,"timeZone":"Europe/London","defaultReminders":[{"method":"popup","minutes":10},{"method":"email","minutes":60}]},
			{"id":"gone","summary":"Gone","deleted":true}]}`))
	}))
	cals, err := googleAdapter{}.Calendars(context.Background(), calendarSyncBusiness.Link{PersonUUID: "p", Account: "sam@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 || !cals[0].Primary || cals[0].Name != "sam@example.com" || cals[0].Zone != "Europe/London" || len(cals[0].Reminders) != 1 || cals[0].Reminders[0] != 10 {
		t.Fatalf("own %+v", cals)
	}
	if cals[1].Name != "Offsites" || cals[1].Zone != "Europe/Berlin" {
		t.Fatalf("other %+v", cals[1])
	}
}

// A pull reads a calendar's series with their changed and cancelled
// instances, guests with their answers, reminders, Meet links and files, as
// the codec's events; a sync token Google no longer knows is a full pull.
func TestGooglePullReadsEventsAsTheCodecDoes(t *testing.T) {
	calls := 0
	withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("singleEvents") != "false" || q.Get("showDeleted") != "true" {
			t.Errorf("asked %s", r.URL)
		}
		if q.Get("syncToken") == "stale" {
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"error":{"code":410,"message":"Sync token is no longer valid"}}`))
			return
		}
		if q.Get("pageToken") == "" {
			w.Write([]byte(`{"nextPageToken":"p2","items":[
				{"id":"rev","iCalUID":"review@google.com","etag":"\"1\"","status":"confirmed","summary":"Weekly design review",
				 "description":"<p>Agenda: <b>pricing</b></p>","location":"Room 4","sequence":2,
				 "start":{"dateTime":"2026-10-06T09:15:00+01:00","timeZone":"Europe/London"},"end":{"dateTime":"2026-10-06T10:00:00+01:00","timeZone":"Europe/London"},
				 "recurrence":["RRULE:FREQ=WEEKLY;BYDAY=TU;COUNT=8","EXDATE;TZID=Europe/London:20261020T091500"],
				 "organizer":{"email":"jonas@example.com","displayName":"Jonas"},
				 "attendees":[{"email":"maya@example.com","displayName":"Maya","responseStatus":"accepted"},{"email":"lee@example.com","optional":true,"responseStatus":"tentative"},{"email":"room4@resource.calendar.google.com","resource":true}],
				 "reminders":{"useDefault":true},
				 "conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+1"},{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]},
				 "attachments":[{"fileUrl":"https://drive.google.com/file/d/1","title":"Deck"}]}]}`))
			return
		}
		w.Write([]byte(`{"nextSyncToken":"tok-2","items":[
			{"id":"rev_20261027T081500Z","iCalUID":"review@google.com","status":"confirmed","recurringEventId":"rev","summary":"Weekly design review (moved)",
			 "originalStartTime":{"dateTime":"2026-10-27T09:15:00Z"},"start":{"dateTime":"2026-10-28T14:00:00Z"},"end":{"dateTime":"2026-10-28T14:45:00Z"},
			 "reminders":{"useDefault":false,"overrides":[{"method":"popup","minutes":30}]}},
			{"id":"rev_20261103T091500Z","iCalUID":"review@google.com","status":"cancelled","recurringEventId":"rev","originalStartTime":{"dateTime":"2026-11-03T09:15:00Z"}},
			{"id":"off","iCalUID":"off@google.com","status":"confirmed","summary":"Day off","transparency":"transparent","start":{"date":"2026-12-24"},"end":{"date":"2026-12-27"}},
			{"id":"gone","status":"cancelled"}]}`))
	}))
	cal := calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{PersonUUID: "p"}, ID: "sam@example.com", Zone: "Europe/London", Reminders: []int{10}}
	changes, next, err := googleAdapter{}.Pull(context.Background(), cal, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "tok-2" || len(changes) != 5 || calls != 2 {
		t.Fatalf("next %q, %d changes over %d calls", next, len(changes), calls)
	}
	series := changes[0].Event
	if series.UID != "review@google.com" || series.Zone != "Europe/London" || !series.Start.Equal(time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC)) || series.RRule != "FREQ=WEEKLY;BYDAY=TU;COUNT=8" || len(series.ExDates) != 1 {
		t.Fatalf("series %+v", series)
	}
	if series.Description != "Agenda: pricing" || series.Conference != "https://meet.google.com/abc-defg-hij" || len(series.Attachments) != 1 || series.Sequence != 2 {
		t.Fatalf("series details %+v", series)
	}
	if len(series.Attendees) != 2 || series.Attendees[0].PartStat != "ACCEPTED" || !series.Attendees[1].Optional || series.Attendees[1].PartStat != "TENTATIVE" || series.Organizer.Email != "jonas@example.com" {
		t.Fatalf("guests %+v %+v", series.Attendees, series.Organizer)
	}
	if len(series.Alarms) != 1 || series.Alarms[0].Before != 10*time.Minute || changes[0].ExternalID != "rev" || changes[0].ETag != `"1"` {
		t.Fatalf("alarms %+v, ids %+v", series.Alarms, changes[0])
	}
	moved := changes[1].Event
	if moved.RecurrenceID == nil || !moved.RecurrenceID.Equal(time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)) || len(moved.Alarms) != 1 || moved.Alarms[0].Before != 30*time.Minute {
		t.Fatalf("moved %+v", moved)
	}
	if changes[2].Deleted || changes[2].Event.Status != "CANCELLED" || changes[2].Event.RecurrenceID == nil {
		t.Fatalf("a cancelled instance is a skipped one, not a deletion: %+v", changes[2])
	}
	if off := changes[3].Event; off.StartDate != "2026-12-24" || off.EndDate != "2026-12-27" || !off.Transparent || off.Zone != "" {
		t.Fatalf("day off %+v", off)
	}
	if !changes[4].Deleted {
		t.Fatalf("a deleted event %+v", changes[4])
	}
	if _, _, err := (googleAdapter{}).Pull(context.Background(), cal, "stale"); !errors.Is(err, calendarSyncBusiness.ErrFullPull) {
		t.Fatalf("a stale token: %v", err)
	}
}

// A push makes OneCamp's change at Google, never emailing guests: a new
// event imported under its own UID, a change patched, one instance by its
// id, and a deletion of what's gone there already is no error.
func TestGooglePushMakesOneCampsChanges(t *testing.T) {
	var asked []string
	withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = append(asked, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/calendars/")+" "+r.URL.Query().Get("sendUpdates")+" "+string(body))
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusGone)
			w.Write([]byte(`{"error":{"code":410}}`))
		case strings.HasSuffix(r.URL.Path, "/import"):
			w.Write([]byte(`{"id":"new1","etag":"\"7\""}`))
		default:
			w.Write([]byte(`{"id":"x","etag":"\"8\""}`))
		}
	}))
	remote := calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{PersonUUID: "p"}, ID: "primary"}
	start := time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC)
	ev := ICS.Event{UID: "abc@onecamp", Summary: "Review", Start: start, End: start.Add(45 * time.Minute), Zone: "Europe/London", RRule: "FREQ=WEEKLY",
		Attendees: []ICS.Attendee{{Address: ICS.Address{Email: "maya@example.com"}, PartStat: ICS.Accepted}}, Alarms: []ICS.Alarm{{Before: 10 * time.Minute}}}
	ref, err := googleAdapter{}.Push(context.Background(), calendarSyncBusiness.Op{Kind: "create", Remote: remote, Event: ev})
	if err != nil || ref.ExternalID != "new1" || ref.ETag != `"7"` {
		t.Fatalf("create %+v %v", ref, err)
	}
	if !strings.Contains(asked[0], `"iCalUID":"abc@onecamp"`) || !strings.Contains(asked[0], `"recurrence":["RRULE:FREQ=WEEKLY"]`) || !strings.Contains(asked[0], `"timeZone":"Europe/London"`) ||
		!strings.Contains(asked[0], `"responseStatus":"accepted"`) || !strings.Contains(asked[0], `"minutes":10`) {
		t.Fatalf("created %s", asked[0])
	}
	rid := start.AddDate(0, 0, 7)
	moved := ev
	moved.RecurrenceID, moved.RRule = &rid, ""
	if _, err := (googleAdapter{}).Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: remote, ExternalID: "new1", Event: moved}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asked[1], "PATCH primary/events/new1_20261013T081500Z none") {
		t.Fatalf("instance patch %s", asked[1])
	}
	if _, err := (googleAdapter{}).Push(context.Background(), calendarSyncBusiness.Op{Kind: "delete", Remote: remote, ExternalID: "new1"}); err != nil {
		t.Fatalf("deleting what's gone: %v", err)
	}
	if !strings.HasPrefix(asked[2], "DELETE primary/events/new1 none") {
		t.Fatalf("delete %s", asked[2])
	}
}

// Google's answers are told apart for sync (C9): an account it no longer
// answers for, a calendar the person may only read, a change it won't take
// as it is, and anything else (a rate limit, an outage) to send again later.
// The calendar list says which calendars can only be read.
func TestGoogleTellsSyncWhyAChangeWasntTaken(t *testing.T) {
	answers := map[string]string{
		"read-only": `403 {"error":{"code":403,"message":"You need to have writer access to this calendar.","errors":[{"reason":"requiredAccessLevel"}]}}`,
		"organiser": `403 {"error":{"code":403,"errors":[{"reason":"forbiddenForNonOrganizer"}]}}`,
		"busy":      `403 {"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`,
		"bad":       `400 {"error":{"code":400,"message":"Invalid recurrence rule."}}`,
		"gone":      `404 {"error":{"code":404,"message":"Not Found"}}`,
		"revoked":   `401 {"error":{"code":401,"message":"Invalid Credentials"}}`,
		"down":      `503 {"error":{"code":503,"message":"Backend Error"}}`,
	}
	withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/users/me/calendarList") {
			w.Write([]byte(`{"items":[{"id":"sam@example.com","primary":true,"accessRole":"owner"},{"id":"team","accessRole":"writer"},{"id":"holidays","accessRole":"reader"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		for summary, answer := range answers {
			if strings.Contains(string(body), `"summary":"`+summary+`"`) {
				code := map[string]int{"400": 400, "401": 401, "403": 403, "404": 404, "503": 503}[answer[:3]]
				w.WriteHeader(code)
				w.Write([]byte(answer[4:]))
				return
			}
		}
		t.Errorf("asked %s %s", r.URL, body)
	}))
	remote := calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{PersonUUID: "p"}, ID: "team"}
	push := func(summary string) error {
		_, err := googleAdapter{}.Push(context.Background(), calendarSyncBusiness.Op{Kind: "create", Remote: remote, Event: ICS.Event{UID: summary + "@onecamp", Summary: summary}})
		return err
	}
	for summary, want := range map[string][3]bool{ // revoked, read-only, refused
		"read-only": {false, true, true},
		"organiser": {false, false, true},
		"bad":       {false, false, true},
		"gone":      {false, false, true},
		"revoked":   {true, false, false},
		"busy":      {false, false, false},
		"down":      {false, false, false},
	} {
		err := push(summary)
		got := [3]bool{errors.Is(err, calendarSyncBusiness.ErrRevoked), errors.Is(err, calendarSyncBusiness.ErrReadOnly), errors.Is(err, calendarSyncBusiness.ErrRefused)}
		if err == nil || got != want {
			t.Errorf("%s: %v reads as revoked, read-only, refused %v, want %v", summary, err, got, want)
		}
	}
	// A refresh Google refused, and a link gone here, are accounts it no
	// longer answers for.
	refused := &url.Error{Op: "Post", URL: "https://oauth2.googleapis.com/token", Err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}}
	if err := syncErr(refused); !errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		t.Errorf("a refused refresh reads as %v", err)
	}
	prev := syncServiceFor
	syncServiceFor = func(context.Context, string) (*calendar.Service, error) { return nil, errNoGoogleLink }
	_, _, err := googleAdapter{}.Pull(context.Background(), remote, "")
	syncServiceFor = prev
	if !errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		t.Errorf("a pull with Google unlinked reads as %v", err)
	}
	cals, err := googleAdapter{}.Calendars(context.Background(), calendarSyncBusiness.Link{PersonUUID: "p"})
	if err != nil || len(cals) != 3 {
		t.Fatalf("calendars %+v %v", cals, err)
	}
	for _, c := range cals {
		if c.ReadOnly != (c.ID == "holidays") {
			t.Errorf("%s read-only %v", c.ID, c.ReadOnly)
		}
	}
}
