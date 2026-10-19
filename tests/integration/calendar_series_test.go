//go:build integration
// +build integration

package integration_test

// Repeating events (calendar A7) in the one store, against real Postgres 12:
// a weekly stand-up over HTTP across the clock change, each scope of a change
// and a delete, the trash and back, an instance its series no longer has,
// and the /event routes by an instance's id. The service's own rules are
// pinned in business/Calendar/series_test.go; these hold the SQL to them.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestRepeatingEvents' -v

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// apiList is who's events from from to to, by the calendar's API.
func apiList(t *testing.T, who *userModels.UserInfo, from, to string) []calendarBusiness.Event {
	t.Helper()
	code, body := v2(t, eventController.ListCalendarEvents, http.MethodGet, "/calendar/events?from="+from+"&to="+to, who, "", "")
	var list []calendarBusiness.Event
	if err := json.Unmarshal(body["data"], &list); code != 200 || err != nil {
		t.Fatalf("list: %d %v %v", code, body, err)
	}
	return list
}

// inLondon is each event as "Tue 20 Oct 09:15 Stand-up (Room 2)".
func inLondon(t *testing.T, es []calendarBusiness.Event) string {
	t.Helper()
	lon, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		s := e.Start.At.In(lon).Format("Mon 2 Jan 15:04") + " " + e.Title
		if e.Location != "" {
			s += " (" + e.Location + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

func TestRepeatingEventsOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	const from, to = "2026-10-19T00:00:00Z", "2026-11-11T00:00:00Z"

	// 09:15 in London on Tuesdays: 08:15 UTC until the clocks go back on 25
	// October, 09:15 UTC after.
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Stand-up","start":{"at":"2026-10-20T08:15:00Z","zone":"Europe/London"},"end":{"at":"2026-10-20T08:30:00Z"},`+
			`"recurrence":{"rule":"FREQ=WEEKLY;BYDAY=TU"},"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	series := dataEvent(t, body)
	if series.Recurrence == nil || series.Recurrence.Text != "Weekly on Tuesday" {
		t.Fatalf("the series: %+v", series)
	}
	in := apiList(t, maya, from, to)
	if s := inLondon(t, in); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, Tue 3 Nov 09:15 Stand-up, Tue 10 Nov 09:15 Stand-up" {
		t.Fatalf("Maya sees %s", s)
	}
	if in[1].ID != series.ID+"_20261027T091500Z" || in[1].SeriesID != series.ID {
		t.Fatalf("an instance: %+v", in[1])
	}
	if got := apiList(t, jonas, from, to); len(got) != 0 {
		t.Errorf("Jonas sees %s", inLondon(t, got))
	}

	patch := func(who *userModels.UserInfo, id, body string) (int, map[string]json.RawMessage) {
		return v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+id, who, body, id)
	}
	seqOf := func(e calendarBusiness.Event) string { return `"sequence":` + itoa(e.Sequence) }

	// This event: one instance.
	if code, body := patch(sam, in[1].ID, `{`+seqOf(in[1])+`,"scope":"this","title":"Stand-up, demo day"}`); code != 200 || dataEvent(t, body).ID != in[1].ID {
		t.Fatalf("this event: %d %v", code, body)
	}
	if code, _ := patch(sam, in[1].ID, `{`+seqOf(in[1])+`,"scope":"this","title":"From an old tab"}`); code != http.StatusConflict {
		t.Errorf("a stale change to an instance: %d", code)
	}
	if code, _ := patch(sam, in[2].ID, `{`+seqOf(in[2])+`,"title":"Which?"}`); code != http.StatusBadRequest {
		t.Errorf("an instance changed with no scope: %d", code)
	}
	if code, _ := patch(maya, in[2].ID, `{`+seqOf(in[2])+`,"scope":"this","title":"Mine"}`); code != http.StatusForbidden {
		t.Errorf("a guest's change: %d", code)
	}

	// This and following: a new series from 3 November.
	code, body = patch(sam, in[2].ID, `{`+seqOf(in[2])+`,"scope":"following","location":"Room 2"}`)
	if code != 200 {
		t.Fatalf("this and following: %d %v", code, body)
	}
	fresh := dataEvent(t, body)
	if fresh.ID == series.ID || fresh.Recurrence == nil || len(fresh.Attendees) != 1 {
		t.Fatalf("the new series: %+v", fresh)
	}
	in = apiList(t, maya, from, to)
	if s := inLondon(t, in); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, demo day, Tue 3 Nov 09:15 Stand-up (Room 2), Tue 10 Nov 09:15 Stand-up (Room 2)" {
		t.Errorf("then Maya sees %s", s)
	}

	// Deleting one instance, the trash, and back.
	del := func(id, query string) int {
		code, _ := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+id+query, sam, "", id)
		return code
	}
	if c := del(in[3].ID, ""); c != http.StatusBadRequest {
		t.Errorf("an instance deleted with no scope: %d", c)
	}
	if c := del(in[3].ID, "?scope=this&sequence="+itoa(in[3].Sequence)); c != 200 {
		t.Fatalf("delete this: %d", c)
	}
	code, body = v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash", sam, "", "")
	var trash []calendarBusiness.Event
	if err := json.Unmarshal(body["data"], &trash); code != 200 || err != nil || len(trash) != 1 || trash[0].ID != in[3].ID {
		t.Fatalf("the trash: %d %v", code, body)
	}
	if code, body := v2(t, eventController.RestoreCalendarEvent, http.MethodPost, "/calendar/events/"+in[3].ID+"/restore", sam, "", in[3].ID); code != 200 || dataEvent(t, body).ID != in[3].ID {
		t.Fatalf("restore: %d %v", code, body)
	}
	if n := len(apiList(t, maya, from, to)); n != 4 {
		t.Errorf("after it came back, Maya sees %d", n)
	}

	// All events, from the first part: the first part goes to the trash.
	if c := del(in[0].ID, "?scope=all"); c != 200 {
		t.Fatalf("delete all: %d", c)
	}
	if s := inLondon(t, apiList(t, maya, from, to)); s != "Tue 3 Nov 09:15 Stand-up (Room 2), Tue 10 Nov 09:15 Stand-up (Room 2)" {
		t.Errorf("the first part deleted, Maya sees %s", s)
	}

	// The /event routes, by an instance's id: that instance alone.
	ref := in[3].ID
	rec := call(t, eventController.GetEventController, http.MethodGet, "/event/getEvent/"+ref, maya, "", map[string]string{"eventId": ref})
	var got struct {
		Data dgraphStruct.DgraphEvent `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != 200 || err != nil || got.Data.Uuid != ref {
		t.Fatalf("the old route opens an instance: %d %s", rec.Code, rec.Body.String())
	}
	old := `{"title":"Last stand-up","startTime":"2026-11-10T09:15:00Z","endTime":"2026-11-10T09:30:00Z","participants":["` + maya.UserDgraphInfo.Uuid + `"]}`
	if rec := call(t, eventController.UpdateEventController, http.MethodPost, "/event/updateEvent/"+ref, sam, old, map[string]string{"eventId": ref}); rec.Code != 200 {
		t.Fatalf("the old route changes an instance: %d %s", rec.Code, rec.Body.String())
	}
	if s := inLondon(t, apiList(t, maya, from, to)); s != "Tue 3 Nov 09:15 Stand-up (Room 2), Tue 10 Nov 09:15 Last stand-up (Room 2)" {
		t.Errorf("after the old route's change, Maya sees %s", s)
	}
	if rec := call(t, eventController.LeaveEventController, http.MethodPost, "/event/leaveEvent/"+ref, maya, "", map[string]string{"eventId": ref}); rec.Code != 200 {
		t.Fatalf("leaving by an instance: %d %s", rec.Code, rec.Body.String())
	}
	if got := apiList(t, maya, from, to); len(got) != 0 {
		t.Errorf("Maya left, and sees %s", inLondon(t, got))
	}
}

// An instance a change to its series leaves behind goes to the trash, and
// comes back as an event of its own, with the series' guests: a row that
// leaves its series in Postgres, with its own iCalendar uid.
func TestRepeatingEventsLeaveNothingBehind(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	const from, to = "2026-10-19T00:00:00Z", "2026-11-01T00:00:00Z"
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Stand-up","start":{"at":"2026-10-20T08:15:00Z","zone":"Europe/London"},"end":{"at":"2026-10-20T08:30:00Z"},`+
			`"recurrence":{"rule":"FREQ=WEEKLY;BYDAY=TU"},"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	series := dataEvent(t, body)
	in := apiList(t, sam, from, to)
	if code, body := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+in[1].ID, sam,
		`{"sequence":`+itoa(in[1].Sequence)+`,"scope":"this","title":"Offsite"}`, in[1].ID); code != 200 {
		t.Fatalf("this event: %d %v", code, body)
	}
	if code, body := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+series.ID, sam,
		`{"sequence":0,"recurrence":{"rule":"FREQ=WEEKLY;BYDAY=WE"}}`, series.ID); code != 200 {
		t.Fatalf("the series on Wednesdays: %d %v", code, body)
	}
	if s := inLondon(t, apiList(t, sam, from, to)); s != "Tue 20 Oct 09:15 Stand-up, Wed 21 Oct 09:15 Stand-up, Wed 28 Oct 09:15 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
	code, body = v2(t, eventController.RestoreCalendarEvent, http.MethodPost, "/calendar/events/"+in[1].ID+"/restore", sam, "", in[1].ID)
	back := dataEvent(t, body)
	if code != 200 || back.SeriesID != "" || back.Recurrence != nil || back.Title != "Offsite" || len(back.Attendees) != 1 || back.ICalUID == series.ICalUID {
		t.Fatalf("back on its own: %d %+v", code, back)
	}
	if s := inLondon(t, apiList(t, maya, from, to)); s != "Tue 20 Oct 09:15 Stand-up, Wed 21 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Offsite, Wed 28 Oct 09:15 Stand-up" {
		t.Errorf("Maya sees %s", s)
	}
	var overrides int
	if err := env.PG.QueryRow(`SELECT count(*) FROM events WHERE series_id = $1`, series.ID).Scan(&overrides); err != nil || overrides != 0 {
		t.Errorf("overrides left on the series: %d %v", overrides, err)
	}
}

// An instance moved by hand past its series' end, or before its start, is
// still found where it went.
func TestRepeatingEventsFindAnInstanceMovedAway(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Review","start":{"at":"2026-11-03T10:00:00Z","zone":"Europe/London"},"end":{"at":"2026-11-03T11:00:00Z"},`+
			`"recurrence":{"rule":"FREQ=WEEKLY;COUNT=2"}}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	in := apiList(t, sam, "2026-11-01T00:00:00Z", "2026-11-15T00:00:00Z")
	if len(in) != 2 {
		t.Fatalf("two reviews: %s", inLondon(t, in))
	}
	for i, at := range []string{"2026-12-01T10:00:00Z", "2026-10-20T10:00:00Z"} {
		end := strings.Replace(at, "T10:", "T11:", 1)
		if code, body := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+in[i].ID, sam,
			`{"sequence":`+itoa(in[i].Sequence)+`,"scope":"this","start":{"at":"`+at+`","zone":"Europe/London"},"end":{"at":"`+end+`"}}`, in[i].ID); code != 200 {
			t.Fatalf("moving %d: %d %v", i, code, body)
		}
	}
	if s := inLondon(t, apiList(t, sam, "2026-11-30T00:00:00Z", "2026-12-02T00:00:00Z")); s != "Tue 1 Dec 10:00 Review" {
		t.Errorf("past the end: %s", s)
	}
	if s := inLondon(t, apiList(t, sam, "2026-10-19T00:00:00Z", "2026-10-21T00:00:00Z")); s != "Tue 20 Oct 11:00 Review" {
		t.Errorf("before the start: %s", s)
	}
	if got := apiList(t, sam, "2026-11-01T00:00:00Z", "2026-11-15T00:00:00Z"); len(got) != 0 {
		t.Errorf("where they were: %s", inLondon(t, got))
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
