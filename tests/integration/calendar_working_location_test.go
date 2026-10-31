//go:build integration
// +build integration

package integration_test

// Calendar heads 5c and 5d (gcal-9b) over HTTP, on Postgres and Dgraph, made
// and read through the routes the web calls. 5d: POST /event/hours with a day
// answers where each person asked about works that day, by their working
// location: the asker's own, a shared one and a weekly one on each day it
// repeats, never someone else's private one (its owner's alone), only the
// asker's own when the workspace keeps busy times private, and 400 for a day
// that isn't one. 5c: a decline made by an out of office carries its message
// and is marked out_of_office in the guest list, in the web's read and the
// calendar's own; one made by hand is not.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestWhereSomeoneWorksOverHTTP|TestAnOutOfOfficeDeclineIsMarkedOverHTTP' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// hoursAnswer is what POST /event/hours answers besides the hours themselves.
type hoursAnswer struct {
	Private []string          `json:"private"`
	Where   map[string]string `json:"where"`
}

// askHours posts /event/hours as asker about people, on day when it is given.
func askHours(t *testing.T, asker *userModels.UserInfo, day string, people ...*userModels.UserInfo) (int, hoursAnswer, string) {
	t.Helper()
	ids := make([]string, len(people))
	for i, p := range people {
		ids[i] = p.UserDgraphInfo.Uuid
	}
	in := map[string]any{"people": ids}
	if day != "" {
		in["day"] = day
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	rec := call(t, eventController.PeopleHours, http.MethodPost, "/event/hours", asker, string(body), nil)
	var out hoursAnswer
	if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("hours that aren't JSON: %s", rec.Body.String())
	}
	return rec.Code, out, rec.Body.String()
}

// makeEvent posts an event to the calendar's own API as who and answers it.
func makeEvent(t *testing.T, who *userModels.UserInfo, body string) calendarBusiness.Event {
	t.Helper()
	code, out := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", who, body, "")
	if code != http.StatusCreated {
		t.Fatalf("%s's event %s: %d %s", who.UserDgraphInfo.UserName, body, code, out["msg"])
	}
	return dataEvent(t, out)
}

func TestWhereSomeoneWorksOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	day := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour)
	d, next := day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")
	allDay := `"start":{"date":"` + d + `"},"end":{"date":"` + next + `"}`

	home := makeEvent(t, sam, `{"kind":"working_location","location":"Home",`+allDay+`}`)
	makeEvent(t, maya, `{"kind":"working_location","location":"Lisbon office",`+allDay+`}`)
	makeEvent(t, jonas, `{"kind":"working_location","location":"Clinic","visibility":"private",`+allDay+`}`)
	// The next day Maya works from the office for the working day alone.
	nine := day.AddDate(0, 0, 1).Add(9 * time.Hour)
	makeEvent(t, maya, `{"kind":"working_location","location":"Office","start":{"at":"`+rfc(nine)+`","zone":"UTC"},"end":{"at":"`+rfc(nine.Add(8*time.Hour))+`"}}`)
	// Lee works from the studio every week, from a week before the day.
	makeEvent(t, lee, `{"kind":"working_location","location":"Studio","start":{"date":"`+day.AddDate(0, 0, -7).Format("2006-01-02")+`"},`+
		`"end":{"date":"`+day.AddDate(0, 0, -6).Format("2006-01-02")+`"},"recurrence":{"rule":"FREQ=WEEKLY"}}`)

	// A working location is its owner's alone and says where, when it is made
	// and at every change: no guests, a place, and never busy.
	sync := makeEvent(t, sam, `{"title":"Sync","start":{"at":"`+rfc(nine)+`","zone":"UTC"},"end":{"at":"`+rfc(nine.Add(time.Hour))+`"},`+
		`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`)
	noGuests, guest := "A working location is yours alone: it has no guests.", `"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]`
	refused := func(what string, code int, out map[string]json.RawMessage, want string) {
		t.Helper()
		var msg string
		if code != http.StatusBadRequest || json.Unmarshal(out["msg"], &msg) != nil || msg != want {
			t.Errorf("%s: %d %s, want 400 %q", what, code, out["msg"], want)
		}
	}
	change := func(e calendarBusiness.Event, body string) (int, map[string]json.RawMessage) {
		return v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam, `{"sequence":`+strconv.Itoa(e.Sequence)+`,`+body+`}`, e.ID)
	}
	code, out := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam, `{"kind":"working_location","location":"Office",`+allDay+`,`+guest+`}`, "")
	refused("made with a guest", code, out, noGuests)
	code, out = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam, `{"kind":"working_location",`+allDay+`}`, "")
	refused("made with no place", code, out, "Say where you're working.")
	code, out = change(home, guest)
	refused("a guest added", code, out, noGuests)
	code, out = change(home, `"title":"","location":""`)
	refused("its place cleared", code, out, "Say where you're working.")
	code, out = change(sync, `"kind":"working_location","location":"Office"`)
	refused("a meeting made a working location", code, out, noGuests)
	if code, out := change(home, `"transparency":"opaque"`); code != http.StatusOK || dataEvent(t, out).Transparency != "transparent" {
		t.Errorf("made busy: %d %s, want it kept free", code, out["msg"])
	}
	// Every kind of own time keeps the same rule: an out of office takes no
	// guests by a change either, and a meeting with guests isn't made one.
	later := nine.Add(48 * time.Hour)
	away := makeEvent(t, sam, `{"kind":"out_of_office","title":"Out of office","start":{"at":"`+rfc(later)+`","zone":"UTC"},"end":{"at":"`+rfc(later.Add(time.Hour))+`"}}`)
	noAwayGuests := "An out of office is yours alone: it has no guests."
	code, out = change(away, guest)
	refused("a guest added to an out of office", code, out, noAwayGuests)
	code, out = change(sync, `"kind":"out_of_office"`)
	refused("a meeting made an out of office", code, out, noAwayGuests)

	// Sam is told his own place, Maya's shared one and Lee's weekly one that
	// day; Jonas's private one is never named.
	code, got, body := askHours(t, sam, d, sam, maya, jonas, lee)
	if want := map[string]string{sam.UserDgraphInfo.Uuid: "Home", maya.UserDgraphInfo.Uuid: "Lisbon office", lee.UserDgraphInfo.Uuid: "Studio"}; code != http.StatusOK ||
		!reflect.DeepEqual(got.Where, want) || strings.Contains(body, "Clinic") {
		t.Errorf("where, to Sam: %d %s", code, body)
	}
	// Jonas is told his own private place.
	if code, got, body := askHours(t, jonas, d, jonas, maya); code != http.StatusOK || got.Where[jonas.UserDgraphInfo.Uuid] != "Clinic" || got.Where[maya.UserDgraphInfo.Uuid] != "Lisbon office" {
		t.Errorf("where, to Jonas: %d %s", code, body)
	}
	// An all-day place ends with its day, a weekly one is back in a week, and
	// a timed one counts on the day it runs.
	if code, got, body := askHours(t, sam, next, sam, maya, lee); code != http.StatusOK || !reflect.DeepEqual(got.Where, map[string]string{maya.UserDgraphInfo.Uuid: "Office"}) {
		t.Errorf("where, the next day: %d %s", code, body)
	}
	// Without a day nothing says where; a day that isn't one is refused.
	if code, got, body := askHours(t, sam, "", sam, maya); code != http.StatusOK || got.Where != nil || strings.Contains(body, `"where"`) {
		t.Errorf("hours without a day: %d %s", code, body)
	}
	if code, _, body := askHours(t, sam, "not-a-day", sam); code != http.StatusBadRequest || !strings.Contains(body, "That isn't a day.") {
		t.Errorf("a day that isn't one: %d %s", code, body)
	}
	// A working location is never busy.
	if busy, err := calendarBusiness.BusyFor(context.Background(), sam.UserDgraphInfo.Uuid, day, day.AddDate(0, 0, 1)); err != nil || len(busy.Intervals) != 0 {
		t.Errorf("Sam busy at home: %+v %v", busy, err)
	}

	// A workspace that keeps when members are busy to themselves keeps where
	// they work too: Sam is told his own place alone, and Maya is named private.
	prev := policyBusiness.Policies
	policyBusiness.Policies = busyKeptPrivate{prev}
	t.Cleanup(func() { policyBusiness.Policies = prev })
	code, got, body = askHours(t, sam, d, sam, maya)
	if code != http.StatusOK || !reflect.DeepEqual(got.Where, map[string]string{sam.UserDgraphInfo.Uuid: "Home"}) ||
		!reflect.DeepEqual(got.Private, []string{maya.UserDgraphInfo.Uuid}) || strings.Contains(body, "Lisbon") {
		t.Errorf("where, kept private, to Sam: %d %s", code, body)
	}
}

// guestMark is one guest's answer as a read shows it.
type guestMark struct {
	Response    string
	Note        string
	OutOfOffice bool
}

func TestAnOutOfOfficeDeclineIsMarkedOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	from := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	makeEvent(t, maya, `{"kind":"out_of_office","title":"Out of office","start":{"at":"`+rfc(from)+`","zone":"UTC"},"end":{"at":"`+rfc(from.Add(72*time.Hour))+`"},`+
		`"out_of_office":{"decline":"new","message":"Back Monday."}}`)

	// Sam invites Maya and Lee to a sync while Maya is away; Lee declines it by hand.
	st := from.Add(26 * time.Hour)
	id := makeEvent(t, sam, `{"title":"Sync","start":{"at":"`+rfc(st)+`","zone":"UTC"},"end":{"at":"`+rfc(st.Add(30*time.Minute))+`"},`+
		`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+lee.UserDgraphInfo.Uuid+`"}]}`).ID
	if code, out := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+id+"/answer", lee, `{"answer":"declined","note":"Clash"}`, id); code != http.StatusOK {
		t.Fatalf("Lee's answer: %d %s", code, out["msg"])
	}

	want := map[string]guestMark{
		maya.UserDgraphInfo.Uuid: {Response: "declined", Note: "Back Monday.", OutOfOffice: true},
		lee.UserDgraphInfo.Uuid:  {Response: "declined", Note: "Clash"},
	}
	// The organiser and another guest read the same marks, in the web's read
	// (GET /event/getEvent/{eventId}) and in the calendar's own.
	for _, reader := range []*userModels.UserInfo{sam, lee} {
		rec := call(t, eventController.GetEventController, http.MethodGet, "/event/getEvent/"+id, reader, "", map[string]string{"eventId": id})
		var web struct {
			Data struct {
				Guests []struct {
					UserUUID    string `json:"user_uuid"`
					Response    string `json:"response"`
					Note        string `json:"note"`
					OutOfOffice bool   `json:"out_of_office"`
				} `json:"event_guests"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &web) != nil {
			t.Fatalf("the web's read, to %s: %d %s", reader.UserDgraphInfo.UserName, rec.Code, rec.Body.String())
		}
		marks := map[string]guestMark{}
		for _, g := range web.Data.Guests {
			marks[g.UserUUID] = guestMark{g.Response, g.Note, g.OutOfOffice}
		}
		for who, w := range want {
			if marks[who] != w {
				t.Errorf("the web's read, to %s: %s is %+v, want %+v", reader.UserDgraphInfo.UserName, who, marks[who], w)
			}
		}

		code, out := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+id, reader, "", id)
		if code != http.StatusOK {
			t.Fatalf("the calendar's read, to %s: %d %s", reader.UserDgraphInfo.UserName, code, out["msg"])
		}
		marks = map[string]guestMark{}
		for _, g := range dataEvent(t, out).Attendees {
			marks[g.UUID] = guestMark{g.Response, g.Note, g.OutOfOffice}
		}
		for who, w := range want {
			if marks[who] != w {
				t.Errorf("the calendar's read, to %s: %s is %+v, want %+v", reader.UserDgraphInfo.UserName, who, marks[who], w)
			}
		}
	}
}
