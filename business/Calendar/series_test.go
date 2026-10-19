package Calendar

// Repeating events over the store in memory: what a team sees of its stand-up
// across a clock change, and what each scope of a change or a delete does to
// the series, its changed instances and the trash. The SQL is held to the same
// answers by tests/integration/calendar_series_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// series makes who's repeating event by rule, from start, minutes long, in
// London, with guests.
func (h *harness) series(who *model.UserInfo, title, rule string, start time.Time, minutes int, guests ...*model.UserInfo) *Event {
	h.t.Helper()
	st, en := timed(start, minutes, "Europe/London")
	in := EventInput{Title: title, Start: st, End: en, Recurrence: &Recurrence{Rule: rule}}
	for _, g := range guests {
		in.Attendees = append(in.Attendees, GuestInput{UUID: g.UserDgraphInfo.Uuid})
	}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// between is what who sees from from to to.
func (h *harness) between(who *model.UserInfo, from, to time.Time) []Event {
	h.t.Helper()
	es, _, err := h.s.Range(context.Background(), who, from, to)
	if err != nil {
		h.t.Fatal(err)
	}
	return es
}

// seen is each event as "Tue 20 Oct 09:15 Stand-up" in loc: all-day ones by
// their date.
func seen(es []Event, loc *time.Location) string {
	var out []string
	for _, e := range es {
		if e.AllDay {
			d, _ := time.Parse(dateLayout, e.Start.Date)
			out = append(out, d.Format("Mon 2 Jan")+" "+e.Title)
			continue
		}
		out = append(out, e.Start.At.In(loc).Format("Mon 2 Jan 15:04")+" "+e.Title)
	}
	return strings.Join(out, ", ")
}

// patchAt is a change to an instance, for scope, at the sequence read.
func patchAt(e Event, scope string) EventPatch {
	return EventPatch{Sequence: seq(e.Sequence), Scope: scope}
}

func whenAt(t time.Time) *When { return &When{At: &t, Zone: "Europe/London"} }

// A weekly stand-up is 09:15 in London on every Tuesday, before and after
// the clocks go back (25 October), each instance by its own id, and only its
// people see it.
func TestAStandUpKeepsItsTimeAcrossTheClockChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	if e.Recurrence == nil || e.Recurrence.Rule != "FREQ=WEEKLY" || e.Recurrence.Text != "Weekly on Tuesday" || e.SeriesID != "" {
		t.Fatalf("the series: %+v", e)
	}
	got := h.between(maya, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 0, 0, 0, 0, lon))
	if s := seen(got, lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, Tue 3 Nov 09:15 Stand-up, Tue 10 Nov 09:15 Stand-up" {
		t.Fatalf("Maya sees %s", s)
	}
	for i, want := range []string{"_20261020T081500Z", "_20261027T091500Z", "_20261103T091500Z", "_20261110T091500Z"} {
		g := got[i]
		if g.ID != e.ID+want || g.SeriesID != e.ID || g.RecurrenceID == nil || !g.RecurrenceID.At.Equal(*g.Start.At) ||
			g.Recurrence == nil || g.Recurrence.Text != "Weekly on Tuesday" || guestNames(&g) != "Maya" || g.Organiser.Name != "Sam" {
			t.Errorf("instance %d: %+v", i, g)
		}
	}
	one, err := h.s.Get(ctx, maya, got[1].ID)
	if err != nil || one.ID != got[1].ID || !one.Start.At.Equal(*got[1].Start.At) {
		t.Errorf("one instance: %+v %v", one, err)
	}
	if _, err := h.s.Get(ctx, sam, e.ID+"_20261021T081500Z"); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("a Wednesday isn't an instance: %v", err)
	}
	if _, err := h.s.Get(ctx, jonas, got[0].ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("Jonas isn't on it: %v", err)
	}
	if whole, err := h.s.Get(ctx, sam, e.ID); err != nil || whole.ID != e.ID || whole.Recurrence == nil {
		t.Errorf("the series by its own id: %+v %v", whole, err)
	}
}

// Events, working locations and task blocks repeat; focus time, time off,
// bookings and reminders don't yet, nor does an event with a copy in Google
// Calendar, nor a rule that can't be read or ends before it starts.
func TestWhatMayRepeat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	st, en := timed(time.Date(2026, 10, 20, 9, 0, 0, 0, lon), 60, "Europe/London")
	var input *EventInputError
	for name, in := range map[string]EventInput{
		"focus":            {Kind: EventFocus, Title: "Focus", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY"}},
		"copied to Google": {Title: "Sync", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY"}, SyncToGoogle: true},
		"unreadable":       {Title: "Sync", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=HOURLY"}},
		"ends first":       {Title: "Sync", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;UNTIL=20261001"}},
	} {
		if _, err := h.s.Create(ctx, sam, in); !errors.As(err, &input) {
			t.Errorf("%s: %v", name, err)
		}
	}
	block, err := h.s.Create(ctx, sam, EventInput{Kind: EventTaskBlock, Title: "Write-up", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=2"}})
	if err != nil || block.Recurrence == nil {
		t.Fatalf("a task block repeats: %+v %v", block, err)
	}
	focus := EventKind(EventFocus)
	if _, err := h.s.Update(ctx, sam, block.ID, EventPatch{Sequence: seq(0), Kind: &focus}); !errors.As(err, &input) {
		t.Errorf("a series made focus time: %v", err)
	}

	// A single event made to repeat, and back.
	one, _ := h.s.Create(ctx, sam, EventInput{Title: "Review", Start: st, End: en})
	got, err := h.s.Update(ctx, sam, one.ID, EventPatch{Sequence: seq(0), Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=3"}})
	if err != nil || got.Recurrence == nil || got.Recurrence.Text != "Daily, 3 times" {
		t.Fatalf("made to repeat: %+v %v", got, err)
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, lon) }
	if n := len(h.between(sam, day(19), day(31))); n != 5 {
		t.Errorf("3 reviews and 2 blocks: %d", n)
	}
	got, err = h.s.Update(ctx, sam, one.ID, EventPatch{Sequence: seq(1), Recurrence: &Recurrence{Rule: ""}})
	if err != nil || got.Recurrence != nil {
		t.Fatalf("made single again: %+v %v", got, err)
	}

	// One with a copy in Google can't repeat.
	copied, _ := h.s.Create(ctx, sam, EventInput{Title: "Copied", Start: st, End: en})
	h.store.events[uuid.MustParse(copied.ID)].GoogleEventID = "g1"
	if _, err := h.s.Update(ctx, sam, copied.ID, EventPatch{Sequence: seq(0), Recurrence: &Recurrence{Rule: "FREQ=DAILY"}}); !errors.As(err, &input) {
		t.Errorf("an event copied to Google: %v", err)
	}
}

// "This event" changes one instance only, as an override that records what
// it changed by hand; a change back follows the series again. Its own
// sequence guards it from then on.
func TestThisEventChangesOneInstance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	inst := h.between(sam, from, to)[1]

	p := patchAt(inst, ScopeThis)
	p.Title = str("Stand-up, demo day")
	got, err := h.s.Update(ctx, sam, inst.ID, p)
	if err != nil || got.ID != inst.ID || got.Title != "Stand-up, demo day" || got.SeriesID != e.ID {
		t.Fatalf("this event: %+v %v", got, err)
	}
	if s := seen(h.between(maya, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, demo day, Tue 3 Nov 09:15 Stand-up" {
		t.Errorf("Maya sees %s", s)
	}
	o, _ := h.store.overrideAt(ctx, uuid.MustParse(e.ID), *inst.Start.At)
	if o == nil || strings.Join(o.Changed, ",") != "title" {
		t.Fatalf("the override: %+v", o)
	}

	// Whoever read the instance before it changed is refused, with it as it is.
	var stale *StaleError
	late := patchAt(inst, ScopeThis)
	late.Location = str("Room 2")
	if _, err := h.s.Update(ctx, sam, inst.ID, late); !errors.As(err, &stale) || stale.Current.ID != inst.ID || stale.Current.Title != "Stand-up, demo day" {
		t.Errorf("a stale change to the instance: %v", err)
	}

	// Its title back as the series has it: it follows the series again.
	back := patchAt(*got, ScopeThis)
	back.Title = str("Stand-up")
	if _, err := h.s.Update(ctx, sam, inst.ID, back); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Title: str("Daily sync")}); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(maya, from, to), lon); s != "Tue 20 Oct 09:15 Daily sync, Tue 27 Oct 09:15 Daily sync, Tue 3 Nov 09:15 Daily sync" {
		t.Errorf("after the series' new title, Maya sees %s", s)
	}

	// What only the whole series has is refused for one instance.
	inst = h.between(sam, from, to)[2]
	var input *EventInputError
	focus := EventKind(EventFocus)
	jonas := h.member("Jonas")
	for name, p := range map[string]EventPatch{
		"its rule":        {Recurrence: &Recurrence{Rule: "FREQ=DAILY"}},
		"its guests":      {Attendees: &[]GuestInput{{UUID: jonas.UserDgraphInfo.Uuid}}},
		"what it is":      {Kind: &focus},
		"what guests may": {GuestsCanModify: func() *bool { b := true; return &b }()},
		"no scope":        {Title: str("Which?")},
	} {
		p.Sequence = seq(inst.Sequence)
		if p.Title == nil {
			p.Scope = ScopeThis
		}
		if _, err := h.s.Update(ctx, sam, inst.ID, p); !errors.As(err, &input) {
			t.Errorf("%s: %v", name, err)
		}
	}
	same := []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}
	ok := EventPatch{Sequence: seq(inst.Sequence), Scope: ScopeThis, Attendees: &same, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"}, Title: str("Sync")}
	if _, err := h.s.Update(ctx, sam, inst.ID, ok); err != nil {
		t.Errorf("the same guests and rule, sent along: %v", err)
	}
	p = patchAt(inst, ScopeThis)
	p.Title = str("Maya's")
	if _, err := h.s.Update(ctx, maya, inst.ID, p); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("a guest changing it: %v", err)
	}
}

// "All events" moves every instance as the one moved, keeps what each
// instance changed by hand, and leaves the rule as it was when the day stays.
func TestAllEventsKeepsChangesByHand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)

	moved := patchAt(in[1], ScopeThis)
	moved.Start, moved.End = whenAt(time.Date(2026, 10, 27, 10, 0, 0, 0, lon)), whenAt(time.Date(2026, 10, 27, 10, 15, 0, 0, lon))
	if _, err := h.s.Update(ctx, sam, in[1].ID, moved); err != nil {
		t.Fatal(err)
	}
	named := patchAt(in[2], ScopeThis)
	named.Title = str("Planning")
	if _, err := h.s.Update(ctx, sam, in[2].ID, named); err != nil {
		t.Fatal(err)
	}
	all := patchAt(in[3], ScopeAll)
	all.Start, all.End = whenAt(time.Date(2026, 11, 10, 11, 0, 0, 0, lon)), whenAt(time.Date(2026, 11, 10, 11, 15, 0, 0, lon))
	got, err := h.s.Update(ctx, sam, in[3].ID, all)
	if err != nil || got.ID != e.ID || got.Recurrence == nil || got.Recurrence.Rule != "FREQ=WEEKLY" {
		t.Fatalf("all events: %+v %v", got, err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 11:00 Stand-up, Tue 27 Oct 10:00 Stand-up, Tue 3 Nov 11:00 Planning, Tue 10 Nov 11:00 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}

	// From the instance moved by hand: what the change sets, it sets there
	// too (its time follows the series again); the others keep their own.
	in = h.between(sam, from, to)
	again := patchAt(in[1], ScopeAll)
	again.Start, again.End = whenAt(time.Date(2026, 10, 27, 12, 0, 0, 0, lon)), whenAt(time.Date(2026, 10, 27, 12, 15, 0, 0, lon))
	again.Title = str("Daily sync")
	if _, err := h.s.Update(ctx, sam, in[1].ID, again); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 12:00 Daily sync, Tue 27 Oct 12:00 Daily sync, Tue 3 Nov 12:00 Planning, Tue 10 Nov 12:00 Daily sync" {
		t.Errorf("then Sam sees %s", s)
	}
}

// Dragged to another day for all events, a weekly series moves its day: its
// rule says Wednesday now, and its changed instances move along.
func TestAllEventsMovedToAnotherDay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	h.series(sam, "Stand-up", "FREQ=WEEKLY;BYDAY=TU", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 12, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	named := patchAt(in[2], ScopeThis)
	named.Title = str("Planning")
	if _, err := h.s.Update(ctx, sam, in[2].ID, named); err != nil {
		t.Fatal(err)
	}
	all := patchAt(in[1], ScopeAll)
	all.Start, all.End = whenAt(time.Date(2026, 10, 28, 9, 15, 0, 0, lon)), whenAt(time.Date(2026, 10, 28, 9, 30, 0, 0, lon))
	got, err := h.s.Update(ctx, sam, in[1].ID, all)
	if err != nil || got.Recurrence.Rule != "FREQ=WEEKLY;BYDAY=WE" || got.Recurrence.Text != "Weekly on Wednesday" {
		t.Fatalf("moved a day: %+v %v", got, err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Wed 21 Oct 09:15 Stand-up, Wed 28 Oct 09:15 Stand-up, Wed 4 Nov 09:15 Planning, Wed 11 Nov 09:15 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
}

// A timed series made all-day from one of its instances, for all events, is
// dates from then on: every instance on its own day, the one moved by hand
// keeping its own time.
func TestAllEventsMadeAllDay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	moved := patchAt(in[2], ScopeThis)
	moved.Start, moved.End = whenAt(time.Date(2026, 11, 3, 16, 0, 0, 0, lon)), whenAt(time.Date(2026, 11, 3, 16, 15, 0, 0, lon))
	if _, err := h.s.Update(ctx, sam, in[2].ID, moved); err != nil {
		t.Fatal(err)
	}
	all := patchAt(in[1], ScopeAll)
	all.Start, all.End = &When{Date: "2026-10-27"}, &When{Date: "2026-10-28"}
	got, err := h.s.Update(ctx, sam, in[1].ID, all)
	if err != nil || !got.AllDay || got.Start.Date != "2026-10-20" || got.Recurrence == nil {
		t.Fatalf("all-day: %+v %v", got, err)
	}
	after := h.between(sam, from, to)
	if s := seen(after, lon); s != "Tue 20 Oct Stand-up, Tue 27 Oct Stand-up, Tue 3 Nov 16:00 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
	if len(after) == 3 && after[1].ID != e.ID+"_20261027" {
		t.Errorf("an all-day instance's id: %s", after[1].ID)
	}
}

// "This and following" ends the series before the instance and starts a new
// one there, sharing the count; the later changed instances go along.
func TestThisAndFollowingStartsANewSeries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY;COUNT=6", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 12, 1, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	retro := patchAt(in[4], ScopeThis)
	retro.Title = str("Retro")
	if _, err := h.s.Update(ctx, sam, in[4].ID, retro); err != nil {
		t.Fatal(err)
	}
	p := patchAt(in[2], ScopeFollowing)
	p.Location = str("Room 2")
	fresh, err := h.s.Update(ctx, sam, in[2].ID, p)
	if err != nil || fresh.ID == e.ID || fresh.Recurrence == nil || fresh.Recurrence.Rule != "FREQ=WEEKLY;COUNT=4" || guestNames(fresh) != "Maya" {
		t.Fatalf("the new series: %+v %v", fresh, err)
	}
	got := h.between(maya, from, to)
	var where []string
	for _, g := range got {
		part := "old"
		if g.SeriesID == fresh.ID {
			part = "new"
		}
		where = append(where, fmt.Sprintf("%s %s %q", part, g.Title, g.Location))
	}
	if s := strings.Join(where, ", "); s != `old Stand-up "", old Stand-up "", new Stand-up "Room 2", new Stand-up "Room 2", new Retro "Room 2", new Stand-up "Room 2"` {
		t.Errorf("Maya sees %s", s)
	}
	old, _ := h.s.Get(ctx, sam, e.ID)
	if old.Recurrence.Rule != "FREQ=WEEKLY;COUNT=2" {
		t.Errorf("the old series: %+v", old.Recurrence)
	}
}

// "This and following" with no repeat ends the series at the instance, which
// becomes an event on its own, keeping what it changed by hand.
func TestThisAndFollowingCanStopTheRepeat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 12, 1, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	room := patchAt(in[2], ScopeThis)
	room.Location = str("Room 9")
	changed, err := h.s.Update(ctx, sam, in[2].ID, room)
	if err != nil {
		t.Fatal(err)
	}
	p := patchAt(*changed, ScopeFollowing)
	p.Title, p.Recurrence = str("Last one"), &Recurrence{Rule: ""}
	last, err := h.s.Update(ctx, sam, in[2].ID, p)
	if err != nil || last.Recurrence != nil || last.SeriesID != "" || last.Title != "Last one" || last.Location != "Room 9" {
		t.Fatalf("on its own: %+v %v", last, err)
	}
	got := h.between(sam, from, to)
	if s := seen(got, lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, Tue 3 Nov 09:15 Last one" {
		t.Errorf("Sam sees %s", s)
	}
	if got[2].ID != last.ID || got[2].SeriesID != "" || got[0].SeriesID != e.ID {
		t.Errorf("the ids: %+v", got)
	}
}

// This and following from the first instance is the whole series: a change
// there, or the repeat stopped there, keeping that instance's own changes.
func TestThisAndFollowingFromTheFirstIsAllOfIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY;COUNT=3", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	p := patchAt(in[0], ScopeFollowing)
	p.Title = str("Sync")
	got, err := h.s.Update(ctx, sam, in[0].ID, p)
	if err != nil || got.ID != e.ID || got.Recurrence.Rule != "FREQ=WEEKLY;COUNT=3" {
		t.Fatalf("from the first: %+v %v", got, err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Sync, Tue 27 Oct 09:15 Sync, Tue 3 Nov 09:15 Sync" {
		t.Errorf("Sam sees %s", s)
	}
	in = h.between(sam, from, to)
	room := patchAt(in[0], ScopeThis)
	room.Location = str("Room 9")
	changed, err := h.s.Update(ctx, sam, in[0].ID, room)
	if err != nil {
		t.Fatal(err)
	}
	stop := patchAt(*changed, ScopeFollowing)
	stop.Recurrence, stop.Title = &Recurrence{}, str("Kick-off")
	single, err := h.s.Update(ctx, sam, in[0].ID, stop)
	if err != nil || single.Recurrence != nil || single.Title != "Kick-off" || single.Location != "Room 9" {
		t.Fatalf("stopped from the first: %+v %v", single, err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Kick-off" {
		t.Errorf("then Sam sees %s", s)
	}
	// The first instance's override is the event now, not a deleted instance.
	if trash, _ := h.s.Trash(ctx, sam, nil); len(trash) != 0 {
		t.Errorf("the trash: %s", seen(trash, lon))
	}
}

// Deleting one instance takes it off the calendar into the trash, by its own
// id, and it comes back from there.
func TestDeletingOneInstanceAndBringingItBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	if err := h.s.Delete(ctx, maya, in[1].ID, ScopeThis, nil); !errors.Is(err, ErrNotEventCreator) {
		t.Errorf("a guest deleting: %v", err)
	}
	var input *EventInputError
	if err := h.s.Delete(ctx, sam, in[1].ID, "", nil); !errors.As(err, &input) {
		t.Errorf("an instance deleted with no scope: %v", err)
	}
	if err := h.s.Delete(ctx, sam, in[1].ID, ScopeThis, seq(in[1].Sequence)); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(maya, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 3 Nov 09:15 Stand-up, Tue 10 Nov 09:15 Stand-up" {
		t.Errorf("Maya sees %s", s)
	}
	trash, err := h.s.Trash(ctx, sam, nil)
	if err != nil || len(trash) != 1 || trash[0].ID != e.ID+"_20261027T091500Z" || trash[0].SeriesID != e.ID {
		t.Fatalf("the trash: %+v %v", trash, err)
	}
	back, err := h.s.Restore(ctx, sam, trash[0].ID)
	if err != nil || back.ID != in[1].ID || back.Status != statusConfirmed {
		t.Fatalf("back: %+v %v", back, err)
	}
	if n := len(h.between(maya, from, to)); n != 4 {
		t.Errorf("after it came back, Maya sees %d", n)
	}
	if _, err := h.s.Restore(ctx, sam, trash[0].ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("brought back twice: %v", err)
	}
}

// Deleting this and following ends the series there; the rest is a series of
// its own in the trash, which brings it all back.
func TestDeletingThisAndFollowingCanComeBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 18, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	retro := patchAt(in[3], ScopeThis)
	retro.Title = str("Retro")
	if _, err := h.s.Update(ctx, sam, in[3].ID, retro); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, sam, in[2].ID, ScopeFollowing, nil); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
	trash, _ := h.s.Trash(ctx, sam, nil)
	if len(trash) != 1 || trash[0].SeriesID != "" || trash[0].Recurrence == nil || !trash[0].Start.At.Equal(*in[2].Start.At) {
		t.Fatalf("the trash: %+v", trash)
	}
	if _, err := h.s.Restore(ctx, sam, trash[0].ID); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, Tue 3 Nov 09:15 Stand-up, Tue 10 Nov 09:15 Retro, Tue 17 Nov 09:15 Stand-up" {
		t.Errorf("after it came back, Sam sees %s", s)
	}
	// All events, from any instance, is the whole series.
	in = h.between(sam, from, to)
	if err := h.s.Delete(ctx, sam, in[1].ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 3 Nov 09:15 Stand-up, Tue 10 Nov 09:15 Retro, Tue 17 Nov 09:15 Stand-up" {
		t.Errorf("the first part deleted, Sam sees %s", s)
	}
	if _, err := h.s.Get(ctx, sam, e.ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("the deleted series: %v", err)
	}
}

// An instance a change to its series left behind goes to the trash, and comes
// back from there as an event on its own, with the series' guests.
func TestAnInstanceItsSeriesNoLongerHasComesBackOnItsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY;BYDAY=TU", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 1, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	off := patchAt(in[1], ScopeThis)
	off.Title = str("Offsite")
	if _, err := h.s.Update(ctx, sam, in[1].ID, off); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Recurrence: &Recurrence{Rule: "FREQ=WEEKLY;BYDAY=WE"}}); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Wed 21 Oct 09:15 Stand-up, Wed 28 Oct 09:15 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
	trash, _ := h.s.Trash(ctx, sam, nil)
	if len(trash) != 1 || trash[0].ID != e.ID+"_20261027T091500Z" || trash[0].Title != "Offsite" {
		t.Fatalf("the trash: %+v", trash)
	}
	back, err := h.s.Restore(ctx, sam, trash[0].ID)
	if err != nil || back.SeriesID != "" || back.Recurrence != nil || back.Title != "Offsite" || guestNames(back) != "Maya" {
		t.Fatalf("back on its own: %+v %v", back, err)
	}
	if s := seen(h.between(maya, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Wed 21 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Offsite, Wed 28 Oct 09:15 Stand-up" {
		t.Errorf("Maya sees %s", s)
	}
}

// An instance several days long, moved away by hand, is gone from the days
// it ran over: its override is found from any of them.
func TestALongInstanceMovedAwayIsGoneFromItsDays(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	h.series(sam, "Offsite", "FREQ=WEEKLY;COUNT=3", time.Date(2026, 11, 2, 9, 0, 0, 0, lon), 3*24*60)
	in := h.between(sam, time.Date(2026, 11, 9, 0, 0, 0, 0, lon), time.Date(2026, 11, 10, 0, 0, 0, 0, lon))
	if len(in) != 1 {
		t.Fatalf("the second offsite: %d", len(in))
	}
	p := patchAt(in[0], ScopeThis)
	p.Start, p.End = whenAt(time.Date(2026, 11, 30, 9, 0, 0, 0, lon)), whenAt(time.Date(2026, 12, 3, 9, 0, 0, 0, lon))
	if _, err := h.s.Update(ctx, sam, in[0].ID, p); err != nil {
		t.Fatal(err)
	}
	wed := h.between(sam, time.Date(2026, 11, 11, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 23, 0, 0, 0, lon))
	if len(wed) != 0 {
		t.Errorf("the Wednesday it moved away from shows %s", seen(wed, lon))
	}
	if moved := h.between(sam, time.Date(2026, 12, 2, 0, 0, 0, 0, lon), time.Date(2026, 12, 2, 23, 0, 0, 0, lon)); len(moved) != 1 {
		t.Errorf("the Wednesday it moved to shows %d", len(moved))
	}
}

// A series that stops repeating is its first event; its changed instances go
// to the trash.
func TestAStoppedSeriesIsAnEventAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	p := patchAt(in[1], ScopeThis)
	p.Title = str("Demo")
	if _, err := h.s.Update(ctx, sam, in[1].ID, p); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Recurrence: &Recurrence{}})
	if err != nil || got.Recurrence != nil {
		t.Fatalf("stopped: %+v %v", got, err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up" {
		t.Errorf("Sam sees %s", s)
	}
	if trash, _ := h.s.Trash(ctx, sam, nil); len(trash) != 1 || trash[0].Title != "Demo" {
		t.Errorf("the trash: %+v", trash)
	}
}

// A change to one series while another change to it is being written is
// refused rather than leaving the other behind: a new changed instance moves
// with nothing it doesn't know of.
func TestTwoChangesToOneSeriesAtOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	racing := &racingStore{memStore: h.store}
	h.s.store = racing
	racing.before = func() {
		p := patchAt(in[2], ScopeThis)
		p.Title = str("Planning")
		if _, err := h.s.Update(ctx, sam, in[2].ID, p); err != nil {
			t.Error(err)
		}
	}
	all := patchAt(in[3], ScopeAll)
	all.Title = str("Daily sync")
	var stale *StaleError
	if _, err := h.s.Update(ctx, sam, in[3].ID, all); !errors.As(err, &stale) || stale.Current.ID != in[3].ID {
		t.Fatalf("the second change: %v", err)
	}
	if _, err := h.s.Update(ctx, sam, in[3].ID, all); err != nil {
		t.Fatalf("again, as it is now: %v", err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct 09:15 Daily sync, Tue 27 Oct 09:15 Daily sync, Tue 3 Nov 09:15 Planning, Tue 10 Nov 09:15 Daily sync" {
		t.Errorf("Sam sees %s", s)
	}
}

// racingStore runs another write just before the next batch is written.
type racingStore struct {
	*memStore
	before func()
}

func (r *racingStore) apply(ctx context.Context, b batch) error {
	if f := r.before; f != nil {
		r.before = nil
		f()
	}
	return r.memStore.apply(ctx, b)
}

// A change to all or following instances from a changed one is checked
// against what its reader saw of it: the instance's own sequence.
func TestAChangeFromAChangedInstanceIsCheckedAgainstIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	p := patchAt(in[1], ScopeThis)
	p.Title = str("Demo")
	changed, err := h.s.Update(ctx, sam, in[1].ID, p)
	if err != nil {
		t.Fatal(err)
	}
	var stale *StaleError
	old := patchAt(in[1], ScopeAll)
	old.Location = str("Room 1")
	if _, err := h.s.Update(ctx, sam, in[1].ID, old); !errors.As(err, &stale) || stale.Current.Title != "Demo" {
		t.Errorf("all events, from what it was before: %v", err)
	}
	now := patchAt(*changed, ScopeAll)
	now.Location = str("Room 1")
	if _, err := h.s.Update(ctx, sam, in[1].ID, now); err != nil {
		t.Errorf("all events, from it as it is: %v", err)
	}
	for _, e := range h.between(sam, from, to) {
		if e.Location != "Room 1" {
			t.Errorf("%s isn't in Room 1", e.Title)
		}
	}
}

// The /event routes see a series as its instances, by their ids, and write to
// one instance alone; leaving one leaves the series.
func TestTheOldRoutesWriteToOneInstance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	old, err := h.s.legacyRange(ctx, sam, from, to)
	if err != nil || len(old) != 3 || old[1].Uuid != e.ID+"_20261027T091500Z" {
		t.Fatalf("the old list: %+v %v", old, err)
	}
	one, err := h.s.legacyGet(ctx, maya, old[1].Uuid)
	if err != nil || one.Uuid != old[1].Uuid || one.Title != "Stand-up" {
		t.Fatalf("one instance: %+v %v", one, err)
	}
	in := adapter.CreateOrUpdateEventInput{Title: "Stand-up, demo", StartTime: rfcTime(*one.StartTime), EndTime: rfcTime(*one.EndTime),
		Participants: []string{maya.UserDgraphInfo.Uuid}}
	if err := h.s.legacyUpdateAt(ctx, old[1].Uuid, in, sam); err != nil {
		t.Fatal(err)
	}
	var input *EventInputError
	in.Participants = []string{jonas.UserDgraphInfo.Uuid}
	if err := h.s.legacyUpdateAt(ctx, old[1].Uuid, in, sam); !errors.As(err, &input) {
		t.Errorf("new guests for one instance: %v", err)
	}
	if err := h.s.legacyDeleteAt(ctx, old[2].Uuid, sam); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(maya, from, to), lon); s != "Tue 20 Oct 09:15 Stand-up, Tue 27 Oct 09:15 Stand-up, demo" {
		t.Errorf("Maya sees %s", s)
	}
	if err := h.s.legacyLeaveAt(ctx, old[0].Uuid, maya); err != nil {
		t.Fatal(err)
	}
	if left := h.between(maya, from, to); len(left) != 0 {
		t.Errorf("Maya left the series, and still sees %s", seen(left, lon))
	}
}

// A guest who says no to a series stays on it, their answer on every
// instance; the organiser sees it on each.
func TestAGuestWhoDeclinesASeriesStaysOnIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(maya, from, to)
	if err := h.s.Respond(ctx, maya, in[1].ID, Declined); err != nil {
		t.Fatal(err)
	}
	if n := len(h.between(maya, from, to)); n != 3 {
		t.Errorf("Maya sees %d instances, not 3", n)
	}
	for _, e := range h.between(sam, from, to) {
		if len(e.Attendees) != 1 || e.Attendees[0].Response != responseDeclined {
			t.Errorf("Sam sees %s: %+v", e.ID, e.Attendees)
		}
	}
}

// Who may see an instance is who may see its series.
func TestAnInstanceIsDecidedAsItsSeries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	ref := e.ID + "_20261027T091500Z"
	for who, want := range map[*model.UserInfo]accessBusiness.Level{sam: accessBusiness.LevelFull, maya: accessBusiness.LevelView, jonas: accessBusiness.LevelNone} {
		d, err := h.s.decideMany(ctx, accessBusiness.Subject{UserUUID: who.UserDgraphInfo.Uuid}, []string{e.ID, ref})
		if err != nil || d[ref].Level != want || d[e.ID].Level != want {
			t.Errorf("%s: %+v %v", who.UserDgraphInfo.UserName, d, err)
		}
	}
}

// An all-day series is dates, everywhere: its instances are known by their
// dates, and the scopes work on it as on a timed one.
func TestAnAllDaySeriesIsDates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	e, err := h.s.Create(ctx, sam, EventInput{Title: "Office day", Start: When{Date: "2026-10-20"}, End: When{Date: "2026-10-21"},
		Recurrence: &Recurrence{Rule: "FREQ=WEEKLY;COUNT=4"}})
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 20, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	if s := seen(in, lon); s != "Tue 20 Oct Office day, Tue 27 Oct Office day, Tue 3 Nov Office day, Tue 10 Nov Office day" {
		t.Fatalf("Sam sees %s", s)
	}
	if in[1].ID != e.ID+"_20261027" || in[1].RecurrenceID == nil || in[1].RecurrenceID.Date != "2026-10-27" {
		t.Errorf("an instance's id: %+v", in[1])
	}
	if err := h.s.Delete(ctx, sam, in[1].ID, ScopeThis, nil); err != nil {
		t.Fatal(err)
	}
	p := patchAt(in[2], ScopeFollowing)
	p.Title = str("Away day")
	if _, err := h.s.Update(ctx, sam, in[2].ID, p); err != nil {
		t.Fatal(err)
	}
	if s := seen(h.between(sam, from, to), lon); s != "Tue 20 Oct Office day, Tue 3 Nov Away day, Tue 10 Nov Away day" {
		t.Errorf("Sam sees %s", s)
	}
}

// The web offers a repeat only where events repeat: the feature says whether
// the events are in the one store, by the name the web reads; not while they
// are moving there. The one store is the default (CAL-DEFAULT): only
// CALENDAR_STORE=graph keeps the old stores, which have no repeats.
func TestTheWebIsToldWhetherEventsRepeat(t *testing.T) {
	if helpers.FeatureNameCalendarRepeats != "calendar_repeats" {
		t.Errorf("the feature is %q, the web reads \"calendar_repeats\"", helpers.FeatureNameCalendarRepeats)
	}
	prev := moves
	t.Cleanup(func() { moves = prev })
	for _, moved := range []bool{true, false} {
		moves = &mover{isDone: func() (bool, error) { return moved, nil }, move: func(context.Context) error { return errors.New("not moving here") },
			now: time.Now, wait: time.Millisecond}
		for store, want := range map[string]bool{"postgres": moved, "graph": false, "": moved} {
			t.Setenv("CALENDAR_STORE", store)
			if got := helpers.FeatureStatus()[helpers.FeatureNameCalendarRepeats]; got != want {
				t.Errorf("CALENDAR_STORE=%q, moved %v: %v", store, moved, got)
			}
		}
	}
}

// An instance's id says its series and its start: a time in UTC for a timed
// series, a date for an all-day one.
func TestAnInstanceIDIsReadExactly(t *testing.T) {
	id := uuid.New()
	at := time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)
	for ref, want := range map[string]*time.Time{
		id.String():                      nil,
		instanceRef(id, at, false):       &at,
		id.String() + "_20261020":        func() *time.Time { d := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC); return &d }(),
		" " + instanceRef(id, at, false): &at,
	} {
		got, rid, err := parseRef(ref)
		if err != nil || got != id || (want == nil) != (rid == nil) || (want != nil && !rid.Equal(*want)) {
			t.Errorf("%q: %v %v %v", ref, got, rid, err)
		}
	}
	for _, bad := range []string{"", "not an id", id.String() + "_", id.String() + "_tuesday", "x_20261020"} {
		if _, _, err := parseRef(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
	if !IsInstanceID(instanceRef(id, at, true)) || IsInstanceID(id.String()) || IsInstanceID("gcal-abc") {
		t.Error("IsInstanceID")
	}
}

// A series moved keeps each instance on its own day, at the new time, across
// a clock change and from a timed series to an all-day one.
func TestAnInstanceMovesWithItsSeries(t *testing.T) {
	lon := zone(t, "Europe/London")
	oldStart := time.Date(2026, 10, 20, 9, 15, 0, 0, lon)
	rid := time.Date(2026, 10, 27, 9, 15, 0, 0, lon)
	if got := rekey(rid, oldStart, lon, time.Date(2026, 10, 20, 10, 0, 0, 0, lon), lon); !got.Equal(time.Date(2026, 10, 27, 10, 0, 0, 0, lon)) {
		t.Errorf("a later time: %s", got)
	}
	if got := rekey(rid, oldStart, lon, time.Date(2026, 10, 21, 9, 15, 0, 0, lon), lon); !got.Equal(time.Date(2026, 10, 28, 9, 15, 0, 0, lon)) {
		t.Errorf("a day on: %s", got)
	}
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if got := rekey(rid, oldStart, lon, day, time.UTC); !got.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all day: %s", got)
	}
	var e models.Event
	start := time.Date(2026, 10, 20, 9, 15, 0, 0, lon)
	placeAt(&e, start, 15*time.Minute, false)
	if !e.StartAt.Equal(start) || e.EndAt.Sub(*e.StartAt) != 15*time.Minute || e.StartDate != "" {
		t.Errorf("placed: %+v", e)
	}
}
