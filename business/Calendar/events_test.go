package Calendar

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"google.golang.org/api/calendar/v3"
)

func person(uid, id string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: uid, Uuid: id}
}

// CAL-BUG-1: an update's guest list is the whole list. Whoever it no longer
// names is taken off; the creator is never their own guest.
func TestAnUpdatesGuestListIsTheWholeList(t *testing.T) {
	ev := &dgraphStruct.DgraphEvent{
		CreatedBy:    person("0x1", "sam"),
		Participants: []*dgraphStruct.DgraphUser{person("0x2", "maya"), person("0x3", "jonas"), person("0x1", "sam")},
	}
	uids := func(ps []*dgraphStruct.DgraphUser) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Uid)
		}
		return out
	}

	add, removed := diffParticipants(ev, []string{"maya", " ana ", "sam", "ana"})
	if !reflect.DeepEqual(add, []string{"ana"}) {
		t.Errorf("added %v, want [ana]: once, and never the creator", add)
	}
	if got := uids(removed); !reflect.DeepEqual(got, []string{"0x3", "0x1"}) {
		t.Errorf("taken off %v, want Jonas (0x3) and the creator's own guest entry (0x1)", got)
	}

	add, removed = diffParticipants(ev, []string{})
	if len(add) != 0 || len(removed) != 3 {
		t.Errorf("an empty list must take everyone off: added %v, taken off %v", add, uids(removed))
	}

	add, removed = diffParticipants(ev, []string{"maya", "jonas"})
	if len(add) != 0 || !reflect.DeepEqual(uids(removed), []string{"0x1"}) {
		t.Errorf("the same guests change nothing but the creator's entry: added %v, taken off %v", add, uids(removed))
	}
}

func TestOnlyTheCreatorAndGuestsAreOnAnEvent(t *testing.T) {
	ev := &dgraphStruct.DgraphEvent{CreatedBy: person("0x1", "sam"), Participants: []*dgraphStruct.DgraphUser{person("0x2", "maya"), nil}}
	if !isEventCreator(ev, "sam") || isEventCreator(ev, "maya") || isEventCreator(ev, "") {
		t.Error("only Sam made it")
	}
	if !isEventParticipant(ev, "maya") || isEventParticipant(ev, "sam") || isEventParticipant(ev, "jonas") || isEventParticipant(nil, "maya") {
		t.Error("only Maya is a guest")
	}
}

// CAL-BUG-2: a Google all-day event is its dates, whatever the zone. Its times
// stay as they were (midnight UTC, the end the day after), for an app from
// before the dates.
func TestAGoogleAllDayEventIsItsDates(t *testing.T) {
	me := &model.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x9", Uuid: "sam"}}
	offsite := &calendar.Event{
		Id: "offsite", Summary: "Offsite",
		Start:   &calendar.EventDateTime{Date: "2026-10-14"},
		End:     &calendar.EventDateTime{Date: "2026-10-15"},
		Creator: &calendar.EventCreator{Self: true},
	}
	got := googleEventItem(offsite, me)
	if got == nil {
		t.Fatal("no item for an all-day event")
	}
	if !got.AllDay || got.StartDate != "2026-10-14" || got.EndDate != "2026-10-15" {
		t.Fatalf("all-day %v %q..%q, want the dates 2026-10-14 to 2026-10-15 (end exclusive)", got.AllDay, got.StartDate, got.EndDate)
	}
	if !got.StartTime.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)) || !got.EndTime.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("times %v..%v, want midnight UTC on 14 Oct to midnight on 15 Oct, as before", got.StartTime, got.EndTime)
	}

	three := &calendar.Event{Id: "trip", Start: &calendar.EventDateTime{Date: "2026-10-14"}, End: &calendar.EventDateTime{Date: "2026-10-17"}}
	got = googleEventItem(three, me)
	if got.EndDate != "2026-10-17" || !got.EndTime.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a three-day event ends %q at %v; want 2026-10-17, the day after its last", got.EndDate, got.EndTime)
	}
	if got.CreatedBy != nil || len(got.Participants) != 1 || got.Participants[0].Uuid != "sam" {
		t.Error("an event someone else made lists the reader as its guest")
	}

	// The answer carries the three fields for an all-day event, and never for
	// a timed one or OneCamp's own.
	raw, _ := json.Marshal(googleEventItem(offsite, me))
	for _, k := range []string{`"event_all_day":true`, `"event_start_date":"2026-10-14"`, `"event_end_date":"2026-10-15"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("the answer lacks %s: %s", k, raw)
		}
	}
	timed := googleEventItem(&calendar.Event{Id: "call", Start: &calendar.EventDateTime{DateTime: "2026-10-14T10:00:00+05:30"}, End: &calendar.EventDateTime{DateTime: "2026-10-14T11:00:00+05:30"}}, me)
	raw, _ = json.Marshal(timed)
	if strings.Contains(string(raw), "event_all_day") || strings.Contains(string(raw), "event_start_date") {
		t.Errorf("a timed event carries all-day fields: %s", raw)
	}
	if timed.StartTime.UTC().Format(time.RFC3339) != "2026-10-14T04:30:00Z" {
		t.Errorf("a timed event's start is %v", timed.StartTime)
	}
	if googleEventItem(&calendar.Event{Id: "x", Start: &calendar.EventDateTime{DateTime: "soon"}, End: &calendar.EventDateTime{}}, me) != nil {
		t.Error("an event whose times can't be read is listed")
	}
}
