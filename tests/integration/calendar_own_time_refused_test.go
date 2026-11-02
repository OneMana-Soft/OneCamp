//go:build integration
// +build integration

package integration_test

// CR-113's S1 over HTTP: the event form offers only the own time the server
// takes. Sam's read of a meeting with Lee as its guest says, in
// own_time_refused, each kind of own time a save keeping Lee can't make it, in
// the words that save is refused with; a PATCH to focus time, an out of office
// or a working location answers 400 in exactly those words. Lee's read says
// nothing (a guest can't change it). With Lee taken off, Sam's read says
// nothing and focus time is taken. The pure rule is pinned in business/Calendar
// (TestAnEventSaysWhichOwnTimeItCantBecome).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheFormIsToldWhichOwnTimeTheServerTakesOverHTTP' -v

import (
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestTheFormIsToldWhichOwnTimeTheServerTakesOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam, lee := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Lee")
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	at := func(d time.Time) string { return d.Format(time.RFC3339) }
	sync := makeEvent(t, sam, `{"title":"Sync","start":{"at":"`+at(start)+`"},"end":{"at":"`+at(start.Add(time.Hour))+`"},`+
		`"attendees":[{"user_uuid":"`+lee.UserDgraphInfo.Uuid+`"}],"notify":"none"}`)

	read := func(who *userModels.UserInfo) calendarBusiness.Event {
		t.Helper()
		var e calendarBusiness.Event
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+sync.ID, who, "", sync.ID)
		if code != http.StatusOK || json.Unmarshal(body["data"], &e) != nil {
			t.Fatalf("GET /calendar/events/%s as %s: %d %s", sync.ID, who.UserDgraphInfo.Uuid, code, body["data"])
		}
		return e
	}
	patch := func(seq int, body string) (int, string) {
		t.Helper()
		code, out := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+sync.ID, sam,
			`{"sequence":`+strconv.Itoa(seq)+`,"notify":"none",`+body+`}`, sync.ID)
		var msg string
		_ = json.Unmarshal(out["msg"], &msg)
		return code, msg
	}

	asSam := read(sam)
	want := map[calendarBusiness.EventKind]string{
		calendarBusiness.EventFocus:           "Focus time is yours alone: it has no guests.",
		calendarBusiness.EventAway:            "An out of office is yours alone: it has no guests.",
		calendarBusiness.EventWorkingLocation: "A working location is yours alone: it has no guests.",
		calendarBusiness.EventReminder:        "A reminder is yours alone: it has no guests.",
		calendarBusiness.EventTaskBlock:       "A task block is yours alone: it has no guests.",
	}
	if !maps.Equal(asSam.OwnTimeRefused, want) {
		t.Errorf("Sam's read of a meeting with a guest: own_time_refused %v, want %v", asSam.OwnTimeRefused, want)
	}
	for _, k := range []calendarBusiness.EventKind{calendarBusiness.EventFocus, calendarBusiness.EventAway, calendarBusiness.EventWorkingLocation} {
		if code, msg := patch(asSam.Sequence, `"kind":"`+string(k)+`"`); code != http.StatusBadRequest || msg != asSam.OwnTimeRefused[k] {
			t.Errorf("PATCH kind %s keeping Lee: %d %q, want 400 in the read's words %q", k, code, msg, asSam.OwnTimeRefused[k])
		}
	}
	if asLee := read(lee); asLee.OwnTimeRefused != nil {
		t.Errorf("Lee's read (a guest): own_time_refused %v, want nothing", asLee.OwnTimeRefused)
	}

	// Lee taken off: nothing refused, and focus time is taken.
	if code, msg := patch(asSam.Sequence, `"attendees":[]`); code != http.StatusOK {
		t.Fatalf("PATCH attendees []: %d %q", code, msg)
	}
	alone := read(sam)
	if alone.OwnTimeRefused != nil {
		t.Errorf("Sam's read with no guests: own_time_refused %v, want nothing", alone.OwnTimeRefused)
	}
	if code, msg := patch(alone.Sequence, `"kind":"focus"`); code != http.StatusOK {
		t.Errorf("PATCH kind focus with no guests: %d %q, want 200", code, msg)
	}
}
