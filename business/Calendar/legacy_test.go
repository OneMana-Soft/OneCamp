package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// asJSON is a value as the /event routes send it, read back as plain JSON.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rfcTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// The old routes answer from the store in the graph's shape, every field the
// web reads where it was: the people named, the flags, the Google id, and
// (calendar A2) each guest's answer.
func TestTheOldRoutesAnswerInTheGraphsShape(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	yes := true
	out, err := h.s.legacyCreate(ctx, sam, adapter.CreateOrUpdateEventInput{Title: "Focus time", Description: "Announcement draft.",
		StartTime: rfcTime(start), EndTime: rfcTime(start.Add(2 * time.Hour)), IsFocus: &yes,
		Participants: []string{maya.UserDgraphInfo.Uuid, sam.UserDgraphInfo.Uuid}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Title != "Focus time" || !out.IsFocus || out.IsAway || out.CreatedBy != sam.UserDgraphInfo.Uuid ||
		!reflect.DeepEqual(out.Participants, []string{maya.UserDgraphInfo.Uuid}) || !out.StartTime.Equal(start) {
		t.Fatalf("the create's answer: %+v", out)
	}
	list, err := h.s.legacyRange(ctx, maya, start.Add(-time.Hour), start.Add(3*time.Hour))
	if err != nil || len(list) != 1 {
		t.Fatalf("Maya's list: %+v %v", list, err)
	}
	got := asJSON(t, list[0])
	samID, mayaID := sam.UserDgraphInfo.Uuid, maya.UserDgraphInfo.Uuid
	want := map[string]any{
		"event_uuid": out.EventUuid, "event_title": "Focus time", "event_description": "Announcement draft.",
		"event_start_time": "2026-10-16T12:00:00Z", "event_end_time": "2026-10-16T14:00:00Z",
		"event_created_by":   map[string]any{"uid": h.people[samID].Uid, "user_uuid": samID, "user_name": "Sam", "user_full_name": "Sam Full"},
		"event_participants": []any{map[string]any{"uid": h.people[mayaID].Uid, "user_uuid": mayaID, "user_name": "Maya", "user_full_name": "Maya Full"}},
		"event_is_focus":     true, "event_is_away": false,
		// Calendar A2: each guest's answer, beside the people the web names.
		"event_guests": []any{map[string]any{"user_uuid": mayaID, "role": "required", "response": "needs_action"}},
		// Calendar A5: the reader's reminders. Maya hasn't answered: none by default.
		"event_reminders": []any{}, "event_reminders_default": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the old shape:\n got %v\nwant %v", got, want)
	}
	// Sam organises it: 10 minutes before, by default.
	want["event_reminders"] = []any{float64(10)}
	one, err := h.s.legacyGet(ctx, sam, out.EventUuid)
	if err != nil || !reflect.DeepEqual(asJSON(t, one), want) {
		t.Errorf("one event: %v %v", asJSON(t, one), err)
	}
}

// CAL3 and acceptance 4: an all-day event is its dates, and the old routes
// send it as the web places Google's all-day events, with its dates and
// midnight-UTC instants, so "Launch day" is the same date in New York and
// India. A zone OneCamp doesn't know leaves it as it was: instants.
func TestAnAllDayEventIsTheSameDateEverywhere(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya, jonas := h.member("Maya"), h.member("Jonas")
	h.zone(maya, "Asia/Kolkata")
	kolkata := zone(t, "Asia/Kolkata")
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, kolkata)
	out, err := h.s.legacyCreate(ctx, maya, adapter.CreateOrUpdateEventInput{Title: "Launch day",
		StartTime: rfcTime(day), EndTime: rfcTime(day.AddDate(0, 0, 1)), Participants: []string{jonas.UserDgraphInfo.Uuid}})
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"Maya", "Jonas"} {
		reader := maya
		if who == "Jonas" {
			reader = jonas
		}
		list, _ := h.s.legacyRange(ctx, reader, day.AddDate(0, 0, -2), day.AddDate(0, 0, 2))
		if len(list) != 1 {
			t.Fatalf("%s: %+v", who, list)
		}
		got := asJSON(t, list[0])
		if got["event_all_day"] != true || got["event_start_date"] != "2026-10-22" || got["event_end_date"] != "2026-10-23" ||
			got["event_start_time"] != "2026-10-22T00:00:00Z" || got["event_end_time"] != "2026-10-23T00:00:00Z" {
			t.Errorf("%s reads %v", who, got)
		}
	}
	v, _ := h.s.Get(ctx, maya, out.EventUuid)
	if !v.AllDay || v.Start.Date != "2026-10-22" || v.End.Date != "2026-10-23" {
		t.Errorf("the API: %+v", v)
	}

	// Someone whose zone OneCamp doesn't know: the instants they sent.
	ana := h.member("Ana")
	out2, _ := h.s.legacyCreate(ctx, ana, adapter.CreateOrUpdateEventInput{Title: "Day off",
		StartTime: rfcTime(day), EndTime: rfcTime(day.AddDate(0, 0, 1))})
	v2, _ := h.s.Get(ctx, ana, out2.EventUuid)
	if v2.AllDay || !v2.Start.At.Equal(day) || v2.Start.Zone != "UTC" {
		t.Errorf("a zone not known: %+v", v2)
	}
}

// The old update is a whole replace: notes left empty are cleared, a guest
// list left out leaves the guests and an empty one takes them all off, the
// flags left out leave the kind. Only the organiser changes it.
func TestTheOldUpdateIsAWholeReplace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	start := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	yes := true
	out, _ := h.s.legacyCreate(ctx, sam, adapter.CreateOrUpdateEventInput{Title: "Copy sync", Description: "Pricing page",
		StartTime: rfcTime(start), EndTime: rfcTime(start.Add(time.Hour)), Participants: []string{maya.UserDgraphInfo.Uuid}, IsAway: &yes})
	id := uuid.MustParse(out.EventUuid)

	move := adapter.CreateOrUpdateEventInput{Title: "Copy sync", StartTime: rfcTime(start.Add(time.Hour)), EndTime: rfcTime(start.Add(2 * time.Hour))}
	if err := h.s.legacyUpdate(ctx, id, move, maya); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("a guest: %v", err)
	}
	if err := h.s.legacyUpdate(ctx, id, move, jonas); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("someone not on it: %v", err)
	}
	if err := h.s.legacyUpdate(ctx, id, move, sam); err != nil {
		t.Fatal(err)
	}
	v, _ := h.s.Get(ctx, sam, out.EventUuid)
	if v.Notes != "" || guestNames(v) != "Maya" || v.Kind != EventAway || !v.Start.At.Equal(start.Add(time.Hour)) {
		t.Errorf("after the move: %+v", v)
	}
	// The old routes name members alone, so a guest invited by email stays.
	if _, err := h.s.Update(ctx, sam, out.EventUuid, EventPatch{Sequence: seq(v.Sequence), Attendees: &[]GuestInput{
		{UUID: maya.UserDgraphInfo.Uuid}, {Email: "alex@example.com", Name: "Alex Kim"}}}); err != nil {
		t.Fatal(err)
	}
	move.Participants = []string{}
	_ = h.s.legacyUpdate(ctx, id, move, sam)
	if v, _ := h.s.Get(ctx, sam, out.EventUuid); len(v.Attendees) != 1 || v.Attendees[0].Email != "alex@example.com" || v.Attendees[0].Name != "Alex Kim" {
		t.Errorf("an empty list: %+v", v.Attendees)
	}
	bad := adapter.CreateOrUpdateEventInput{Title: "x", StartTime: rfcTime(start), EndTime: rfcTime(start.Add(-time.Minute))}
	var input *EventInputError
	if err := h.s.legacyUpdate(ctx, id, bad, sam); !errors.As(err, &input) {
		t.Errorf("an end before the start: %v", err)
	}
}

// An all-day event moved by someone in another zone stays all-day, on the
// day they moved it to; moved into the hours, it becomes timed.
func TestAnAllDayEventMovedFromElsewhereStaysAllDay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya := h.member("Maya")
	h.zone(maya, "Asia/Kolkata")
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, zone(t, "Asia/Kolkata"))
	out, _ := h.s.legacyCreate(ctx, maya, adapter.CreateOrUpdateEventInput{Title: "Launch day",
		StartTime: rfcTime(day), EndTime: rfcTime(day.AddDate(0, 0, 1))})
	id := uuid.MustParse(out.EventUuid)
	// Maya is in New York this week; she drags it to the 23rd there.
	ny := time.Date(2026, 10, 23, 0, 0, 0, 0, zone(t, "America/New_York"))
	if err := h.s.legacyUpdate(ctx, id, adapter.CreateOrUpdateEventInput{Title: "Launch day",
		StartTime: rfcTime(ny), EndTime: rfcTime(ny.AddDate(0, 0, 1))}, maya); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.s.Get(ctx, maya, out.EventUuid); !v.AllDay || v.Start.Date != "2026-10-23" || v.End.Date != "2026-10-24" {
		t.Errorf("moved to the 23rd from New York: %+v", v)
	}
	_ = h.s.legacyUpdate(ctx, id, adapter.CreateOrUpdateEventInput{Title: "Launch day",
		StartTime: rfcTime(ny.Add(9 * time.Hour)), EndTime: rfcTime(ny.Add(10 * time.Hour))}, maya)
	if v, _ := h.s.Get(ctx, maya, out.EventUuid); v.AllDay || !v.Start.At.Equal(ny.Add(9*time.Hour)) {
		t.Errorf("moved into the hours: %+v", v)
	}
}

// Notes written as HTML through the API stay HTML when the web saves them
// back through the old routes, through the codec again.
func TestHTMLNotesStayHTMLThroughTheOldRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	st, en := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 30, "UTC")
	e, _ := h.s.Create(ctx, sam, EventInput{Title: "Agenda", NotesFormat: notesHTML, Notes: "<p>One</p>", Start: st, End: en})
	_ = h.s.legacyUpdate(ctx, uuid.MustParse(e.ID), adapter.CreateOrUpdateEventInput{Title: "Agenda",
		Description: `<p>One</p><p onclick="x()">Two</p>`, StartTime: rfcTime(*st.At), EndTime: rfcTime(*en.At)}, sam)
	v, _ := h.s.Get(ctx, sam, e.ID)
	if v.NotesFormat != notesHTML || v.Notes != "<p>One</p><p>Two</p>" {
		t.Errorf("notes %q (%s)", v.Notes, v.NotesFormat)
	}
}

// The calendar's list carries each event's colour and place (the Event
// Form), so the grid draws it in its own colour: a guest's copy too.
func TestTheListCarriesAnEventsColourAndPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 30, "UTC")
	if _, err := h.s.Create(ctx, sam, EventInput{Title: "Offsite", Hue: "moss", Location: "Studio, second floor", Start: st, End: en,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []*model.UserInfo{sam, maya} {
		list, err := h.s.legacyRange(ctx, who, st.At.Add(-time.Hour), st.At.Add(time.Hour))
		if err != nil || len(list) != 1 || list[0].Hue != "moss" || list[0].Location != "Studio, second floor" {
			t.Errorf("%s reads %+v %v", who.UserDgraphInfo.UserName, list, err)
		}
	}
}

// Busy is what shows as busy and isn't cancelled, a guest's events
// included; an all-day day is the whole day in its own zone.
func TestBusyIsWhatShowsAsBusy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 60, "UTC")
	busy := h.create(sam, "Board", maya)
	_ = busy
	free, _ := h.s.Create(ctx, maya, EventInput{Title: "Hold", Transparency: transparencyTransparent, Start: st, End: en})
	day, _ := h.s.Create(ctx, maya, EventInput{Title: "Offsite", Start: When{Date: "2026-10-17", Zone: "Asia/Kolkata"}, End: When{Date: "2026-10-18"}})
	events, err := h.s.busyEvents(ctx, maya.UserDgraphInfo.Uuid, st.At.Add(-time.Hour), st.At.AddDate(0, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, e := range events {
		ids[e.Uuid] = true
		if e.Uuid == day.ID {
			kolkataMidnight := time.Date(2026, 10, 17, 0, 0, 0, 0, zone(t, "Asia/Kolkata"))
			if !e.StartTime.Equal(kolkataMidnight) || !e.EndTime.Equal(kolkataMidnight.AddDate(0, 0, 1)) {
				t.Errorf("the offsite is busy %v to %v", e.StartTime, e.EndTime)
			}
		}
		if e.Title != "" && e.CreatedBy != nil && e.CreatedBy.UserName != "" {
			t.Error("busy names people")
		}
	}
	if !ids[busy.ID] || !ids[day.ID] || ids[free.ID] {
		t.Errorf("busy %v", ids)
	}
}

// The workload's time off is the people shown's, and no one else's; a day
// off is the whole day in its zone.
func TestTimeOffIsThePeopleShownsOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya, jonas := h.member("Maya"), h.member("Jonas")
	away := func(who *model.UserInfo, date, after string) {
		if _, err := h.s.Create(ctx, who, EventInput{Kind: EventAway, Title: "Leave",
			Start: When{Date: date, Zone: "Europe/London"}, End: When{Date: after}}); err != nil {
			t.Fatal(err)
		}
	}
	away(maya, "2026-10-19", "2026-10-21")
	away(jonas, "2026-10-19", "2026-10-20")
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	spans, err := h.s.awaySpans(ctx, []string{maya.UserDgraphInfo.Uuid}, from, from.AddDate(0, 0, 7))
	if err != nil || len(spans) != 1 || spans[0].Person != maya.UserDgraphInfo.Uuid {
		t.Fatalf("%+v %v", spans, err)
	}
	london := zone(t, "Europe/London")
	if want := time.Date(2026, 10, 19, 0, 0, 0, 0, london); !spans[0].Start.Equal(want) ||
		!spans[0].End.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, london)) {
		t.Errorf("Maya away %v to %v", spans[0].Start, spans[0].End)
	}
}

// The resolver's event rule over the store: the organiser manages, a guest
// sees, anyone else nothing; a trashed event is nobody's now, and says what
// each had.
func TestTheStoreDecidesWhoMaySeeAnEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Board", maya)
	gone := h.create(sam, "Old", maya)
	_ = h.s.Delete(ctx, sam, gone.ID, "", nil)
	decide := func(who *model.UserInfo) map[string]accessBusiness.Decision {
		d, err := h.s.decideMany(ctx, accessBusiness.Subject{UserUUID: who.UserDgraphInfo.Uuid}, []string{e.ID, gone.ID, "not an id", uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := decide(sam); d[e.ID].Level != accessBusiness.LevelFull || !d[e.ID].Owner || !d[gone.ID].Deleted ||
		d[gone.ID].Level != accessBusiness.LevelNone || d[gone.ID].Was != accessBusiness.LevelFull || len(d) != 2 {
		t.Errorf("Sam: %+v", d)
	}
	if d := decide(maya); d[e.ID].Level != accessBusiness.LevelView || d[e.ID].Owner {
		t.Errorf("Maya: %+v", d)
	}
	if d := decide(jonas); d[e.ID].Level != accessBusiness.LevelNone {
		t.Errorf("Jonas: %+v", d)
	}
}
