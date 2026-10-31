package Calendar

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func (h *harness) workingAt(who *model.UserInfo, place string, start, end When, visibility ...string) *Event {
	h.t.Helper()
	in := EventInput{Kind: EventWorkingLocation, Location: place, Start: start, End: end}
	if len(visibility) > 0 {
		in.Visibility = visibility[0]
	}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// A working location (head 5d, gcal-9b) is its owner's alone, never busy,
// named by its place, and the web's read carries the place.
func TestAWorkingLocationIsFreeTheOwnersAloneAndNamedByItsPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya, lee := h.member("Maya"), h.member("Lee")
	e := h.workingAt(maya, "Home", When{Date: "2026-10-29"}, When{Date: "2026-10-30"})
	if e.Title != "Home" || e.Location != "Home" || e.Transparency != transparencyTransparent {
		t.Fatalf("made: title %q place %q show as %q, want Home, Home, transparent", e.Title, e.Location, e.Transparency)
	}
	var in *EventInputError
	_, err := h.s.Create(ctx, maya, EventInput{Kind: EventWorkingLocation, Location: "Office", Start: When{Date: "2026-10-29"}, End: When{Date: "2026-10-30"},
		Attendees: []GuestInput{{UUID: lee.UserDgraphInfo.Uuid}}})
	if !errors.As(err, &in) {
		t.Fatalf("with a guest: %v, want refused", err)
	}
	if _, err := h.s.Create(ctx, maya, EventInput{Kind: EventWorkingLocation, Start: When{Date: "2026-10-29"}, End: When{Date: "2026-10-30"}}); !errors.As(err, &in) {
		t.Fatalf("with no place: %v, want refused", err)
	}
	old, err := h.s.legacyGet(ctx, maya, e.ID)
	if err != nil || old.WorkingLocation != "Home" {
		t.Fatalf("the web's read: %+v %v, want event_working_location Home", old, err)
	}
}

// Where people work on a day is their working location that day, all day
// or timed; someone with none, or one another day, isn't named; and it
// follows the one rule: kept private, only the asker's own is answered.
func TestWhereSomeoneWorksIsTheirWorkingLocationThatDay(t *testing.T) {
	h := newHarness(t)
	maya, lee, kim := h.member("Maya"), h.member("Lee"), h.member("Kim")
	h.workingAt(maya, "Home", When{Date: "2026-10-29"}, When{Date: "2026-10-30"})
	from, to := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	h.workingAt(lee, "Lisbon office", When{At: &from, Zone: "UTC"}, When{At: &to})
	h.workingAt(kim, "Home", When{Date: "2026-10-30"}, When{Date: "2026-10-31"})
	people := []uuid.UUID{personOf(maya), personOf(lee), personOf(kim)}
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

	prevStored, prevPolicy := stored, freeBusyPolicy
	t.Cleanup(func() { stored, freeBusyPolicy = prevStored, prevPolicy })
	stored = h.s
	freeBusyPolicy = func(context.Context) bool { return true }
	got, err := WhereSeenBy(context.Background(), maya.UserDgraphInfo.Uuid, people, day)
	want := map[string]string{maya.UserDgraphInfo.Uuid: "Home", lee.UserDgraphInfo.Uuid: "Lisbon office"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("shared: %v %v, want %v", got, err, want)
	}

	freeBusyPolicy = func(context.Context) bool { return false }
	got, err = WhereSeenBy(context.Background(), lee.UserDgraphInfo.Uuid, people, day)
	want = map[string]string{lee.UserDgraphInfo.Uuid: "Lisbon office"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("kept private, asked by Lee: %v %v, want only Lee's own %v", got, err, want)
	}

	// A working location its owner marked private is theirs alone (CR-68 MF1):
	// another member isn't told where they work, the owner is.
	freeBusyPolicy = func(context.Context) bool { return true }
	sam := h.member("Sam")
	h.workingAt(sam, "Clinic", When{Date: "2026-10-29"}, When{Date: "2026-10-30"}, visibilityPrivate)
	got, err = WhereSeenBy(context.Background(), maya.UserDgraphInfo.Uuid, []uuid.UUID{personOf(sam)}, day)
	if err != nil || len(got) != 0 {
		t.Fatalf("Sam's private one, asked by Maya: %v %v, want nothing", got, err)
	}
	got, err = WhereSeenBy(context.Background(), sam.UserDgraphInfo.Uuid, []uuid.UUID{personOf(sam)}, day)
	if err != nil || got[sam.UserDgraphInfo.Uuid] != "Clinic" {
		t.Fatalf("Sam's private one, asked by Sam: %v %v, want Clinic", got, err)
	}
}

// A working location that repeats says where its owner works on each day it
// repeats, not on its first alone: a weekly "Home" from Monday 19 October is
// told on Monday 26 October and not on the Tuesday, one Monday moved to the
// office says "Office", and a Monday deleted says nothing.
func TestARepeatingWorkingLocationIsToldOnEachDayItRepeats(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya := h.member("Maya")
	weekly, err := h.s.Create(ctx, maya, EventInput{Kind: EventWorkingLocation, Location: "Home", Start: When{Date: "2026-10-19"}, End: When{Date: "2026-10-20"},
		Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"}})
	if err != nil {
		t.Fatal(err)
	}
	office := "Office"
	moved := patchAt(*weekly, ScopeThis)
	moved.Location = &office
	if _, err := h.s.Update(ctx, maya, weekly.ID+"_20261102", moved); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, maya, weekly.ID+"_20261109", ScopeThis, seq(weekly.Sequence)); err != nil {
		t.Fatal(err)
	}

	prevStored, prevPolicy := stored, freeBusyPolicy
	t.Cleanup(func() { stored, freeBusyPolicy = prevStored, prevPolicy })
	stored = h.s
	freeBusyPolicy = func(context.Context) bool { return true }
	for _, c := range []struct{ day, want string }{{"2026-10-19", "Home"}, {"2026-10-26", "Home"}, {"2026-10-27", ""}, {"2026-11-02", "Office"}, {"2026-11-09", ""}} {
		day, _ := time.Parse(dateLayout, c.day)
		got, err := WhereSeenBy(ctx, maya.UserDgraphInfo.Uuid, []uuid.UUID{personOf(maya)}, day)
		if err != nil || got[maya.UserDgraphInfo.Uuid] != c.want {
			t.Errorf("%s: %v %v, want %q", c.day, got, err, c.want)
		}
	}
}

// A change keeps a working location to the rules it was made by, through the
// calendar's own API and the /event routes alike: it takes no guests, keeps a
// place, and stays free; and an event with guests can't be made one.
func TestAChangeKeepsAWorkingLocationToItsRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	maya, lee := h.member("Maya"), h.member("Lee")
	home := func() *Event { return h.workingAt(maya, "Home", When{Date: "2026-10-29"}, When{Date: "2026-10-30"}) }
	refused := func(what string, err error, want string) {
		t.Helper()
		var in *EventInputError
		if !errors.As(err, &in) || in.Error() != want {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	guests := []GuestInput{{UUID: lee.UserDgraphInfo.Uuid}}
	e := home()
	_, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(e.Sequence), Attendees: &guests})
	refused("a guest added", err, "A working location is yours alone: it has no guests.")
	blank := ""
	e = home()
	_, err = h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(e.Sequence), Title: &blank, Location: &blank})
	refused("its place cleared", err, "Say where you're working.")

	busy, office := transparencyOpaque, "Office"
	e = home()
	got, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(e.Sequence), Transparency: &busy, Location: &office})
	if err != nil || got.Transparency != transparencyTransparent || got.Location != "Office" {
		t.Errorf("made busy and moved: %+v %v, want free at the office", got, err)
	}

	from, to := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	sync, err := h.s.Create(ctx, maya, EventInput{Title: "Sync", Start: When{At: &from, Zone: "UTC"}, End: When{At: &to}, Attendees: guests})
	if err != nil {
		t.Fatal(err)
	}
	place := EventWorkingLocation
	_, err = h.s.Update(ctx, maya, sync.ID, EventPatch{Sequence: seq(sync.Sequence), Kind: &place, Location: &office})
	refused("a meeting made a working location", err, "A working location is yours alone: it has no guests.")

	timed := h.workingAt(maya, "Studio", When{At: &from, Zone: "UTC"}, When{At: &to})
	err = h.s.legacyUpdateAt(ctx, timed.ID, adapter.CreateOrUpdateEventInput{Title: "Studio", StartTime: from.Format(time.RFC3339), EndTime: to.Format(time.RFC3339),
		Participants: []string{lee.UserDgraphInfo.Uuid}}, maya)
	refused("a guest added through /event/updateEvent", err, "A working location is yours alone: it has no guests.")
}
