//go:build integration
// +build integration

package integration_test

// The Event Form's rules in the one store (calendar FORM), against real
// Postgres 12 over HTTP: a guest made optional keeps their answer, on an
// event and on a series; a guest the organiser lets invite others adds a
// guest and nothing else, and can't once it's off; the calendar's list
// carries each event's colour and place. The service's rules are pinned in
// business/Calendar/guestRoles_test.go; these hold the SQL to them.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheEventFormsGuestRulesOverHTTP' -v

import (
	"net/http"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// guestRole is who's role and answer on e, as the API gives them.
func guestRole(e calendarBusiness.Event, who *userModels.UserInfo) (string, string) {
	for _, a := range e.Attendees {
		if a.UUID == who.UserDgraphInfo.Uuid {
			return a.Role, a.Response
		}
	}
	return "", ""
}

func TestTheEventFormsGuestRulesOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	when := `"start":{"at":"` + rfc(start) + `","zone":"Europe/London"},"end":{"at":"` + rfc(start.Add(time.Hour)) + `"}`
	guest := func(who *userModels.UserInfo, role string) string {
		if role == "" {
			return `{"user_uuid":"` + who.UserDgraphInfo.Uuid + `"}`
		}
		return `{"user_uuid":"` + who.UserDgraphInfo.Uuid + `","role":"` + role + `"}`
	}

	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","hue":"moss","location":"Studio, second floor",`+when+`,"attendees":[`+guest(maya, "")+`]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if code, body := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/answer", maya, `{"answer":"accepted"}`, e.ID); code != http.StatusOK {
		t.Fatalf("Maya's answer: %d %v", code, body)
	}

	// Sam makes Maya optional: her answer stays.
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam,
		`{"sequence":`+itoa(e.Sequence)+`,"attendees":[`+guest(maya, "optional")+`]}`, e.ID)
	if code != http.StatusOK {
		t.Fatalf("optional: %d %v", code, body)
	}
	e = dataEvent(t, body)
	if role, answer := guestRole(e, maya); role != "optional" || answer != "accepted" {
		t.Fatalf("Maya is %q, answered %q", role, answer)
	}

	// Maya (guests may invite others, by default) adds Jonas: nothing else.
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, maya,
		`{"sequence":`+itoa(e.Sequence)+`,"attendees":[`+guest(maya, "")+`,`+guest(jonas, "")+`]}`, e.ID)
	if code != http.StatusOK {
		t.Fatalf("Maya invites Jonas: %d %v", code, body)
	}
	e = dataEvent(t, body)
	if role, _ := guestRole(e, jonas); role != "required" {
		t.Fatalf("Jonas is %q", role)
	}
	if role, answer := guestRole(e, maya); role != "optional" || answer != "accepted" {
		t.Errorf("Maya's own place changed: %q %q", role, answer)
	}
	if code, _ := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, maya,
		`{"sequence":`+itoa(e.Sequence)+`,"attendees":[`+guest(maya, "")+`]}`, e.ID); code != http.StatusForbidden {
		t.Errorf("Maya took Jonas off: %d", code)
	}

	// With inviting off, she can't.
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam,
		`{"sequence":`+itoa(e.Sequence)+`,"guests_can_invite":false}`, e.ID)
	if code != http.StatusOK {
		t.Fatalf("inviting off: %d %v", code, body)
	}
	e = dataEvent(t, body)
	if code, _ := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, maya,
		`{"sequence":`+itoa(e.Sequence)+`,"attendees":[`+guest(maya, "")+`,`+guest(jonas, "")+`,`+guest(lee, "")+`]}`, e.ID); code != http.StatusForbidden {
		t.Errorf("with inviting off, Maya invited Lee: %d", code)
	}

	// A series: Jonas made optional for all of it.
	code, body = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Stand-up",`+when+`,"recurrence":{"rule":"FREQ=WEEKLY"},"attendees":[`+guest(jonas, "")+`]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("series: %d %v", code, body)
	}
	s := dataEvent(t, body)
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+s.ID, sam,
		`{"sequence":`+itoa(s.Sequence)+`,"attendees":[`+guest(jonas, "optional")+`]}`, s.ID)
	if code != http.StatusOK {
		t.Fatalf("the series' guest optional: %d %v", code, body)
	}
	if role, _ := guestRole(dataEvent(t, body), jonas); role != "optional" {
		t.Errorf("Jonas on the series is %q", role)
	}

	// The calendar's list carries the event's colour and place, for a guest too.
	var hue, location string
	if err := env.PG.QueryRow(`SELECT hue, location FROM events WHERE id = $1`, e.ID).Scan(&hue, &location); err != nil || hue != "moss" || location != "Studio, second floor" {
		t.Errorf("stored %q %q %v", hue, location, err)
	}
}
