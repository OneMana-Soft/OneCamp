package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

func roomEvent(title string, start time.Time, minutes int) *models.Event {
	end := start.Add(time.Duration(minutes) * time.Minute)
	return &models.Event{ID: uuid.New(), Title: title, StartAt: &start, EndAt: &end}
}

// A booking clashes when an occurrence overlaps another's; touching ends
// don't.
func TestARoomClashIsAnOverlapNotATouch(t *testing.T) {
	ten := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	other := roomEvent("Budget review", ten, 60)
	theirs := []roomSpan{{start: ten, end: ten.Add(time.Hour), event: other}}
	for name, c := range map[string]struct {
		at    time.Time
		clash bool
	}{"inside": {ten.Add(30 * time.Minute), true}, "before, touching": {ten.Add(-30 * time.Minute), false},
		"after, touching": {ten.Add(time.Hour), false}, "across the start": {ten.Add(-15 * time.Minute), true}} {
		got := firstClash([]instance{{Event: roomEvent("Mine", c.at, 30)}}, theirs)
		if (got != nil) != c.clash || (got != nil && got.event != other) {
			t.Errorf("%s: clash %v, want %v", name, got != nil, c.clash)
		}
	}
}

// A booking spans the event; a series to its last end; an endless series
// has no end; an occurrence moved outside widens it.
func TestABookingSpansEveryOccurrence(t *testing.T) {
	ten := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	one := roomEvent("One", ten, 30)
	if s, e := bookingSpan(one); !s.Equal(ten) || e == nil || !e.Equal(ten.Add(30*time.Minute)) {
		t.Errorf("a single event: %v %v", s, e)
	}
	weekly := roomEvent("Weekly", ten, 30)
	weekly.RRule = "FREQ=WEEKLY"
	if _, e := bookingSpan(weekly); e != nil {
		t.Errorf("an endless series ends at %v", e)
	}
	until := ten.AddDate(0, 0, 21)
	weekly.UntilAt = &until
	_, e := bookingSpan(weekly)
	if e == nil || !e.Equal(until.Add(30*time.Minute)) {
		t.Fatalf("a series to its last end: %v", e)
	}
	// An all-day series ends where its last date ends in its own zone: in
	// Los Angeles, 8 hours after that date's UTC midnight (B100 r2's R1).
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	lastDay := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	days := &models.Event{ID: uuid.New(), Title: "Training days", StartDate: "2026-11-26", EndDate: "2026-11-27", TZ: la.String(), RRule: "FREQ=DAILY;COUNT=3", UntilAt: &lastDay}
	if s, e := bookingSpan(days); !s.Equal(time.Date(2026, 11, 26, 0, 0, 0, 0, la)) || e == nil || !e.Equal(time.Date(2026, 11, 29, 0, 0, 0, 0, la)) {
		t.Errorf("an all-day series in Los Angeles spans %v to %v", s, e)
	}
	moved := []instance{{Event: roomEvent("Moved", until.AddDate(0, 0, 2), 30)}, {Event: roomEvent("Early", ten.Add(-time.Hour), 30)}}
	s, e := widenSpan(ten, e, moved)
	if !s.Equal(ten.Add(-time.Hour)) || !e.Equal(until.AddDate(0, 0, 2).Add(30*time.Minute)) {
		t.Errorf("widened: %v %v", s, e)
	}
	if _, e := widenSpan(ten, nil, moved); e != nil {
		t.Errorf("an endless booking got an end: %v", e)
	}
}

// Someone who may not see the event sees busy only: no title, no event and
// no room name, in a room's calendar and in the event itself.
func TestARoomsBookingIsBusyOnlyToANonViewer(t *testing.T) {
	ten := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	sp := roomSpan{start: ten, end: ten.Add(time.Hour), event: roomEvent("Layoffs", ten, 60), title: "Layoffs"}
	rooms := []Room{{ID: uuid.NewString(), Name: "Board room"}}
	if v := bookingView(sp, rooms, false); !v.Busy || v.Title != "" || v.EventID != "" || len(v.Rooms) != 0 || !v.Start.Equal(ten) {
		t.Errorf("a non-viewer's view: %+v", v)
	}
	if v := bookingView(sp, rooms, true); v.Busy || v.Title != "Layoffs" || len(v.Rooms) != 1 {
		t.Errorf("a viewer's view: %+v", v)
	}
	if v := busyView(Event{Title: "Layoffs", Rooms: rooms}); v.Title != "Busy" || len(v.Rooms) != 0 {
		t.Errorf("the event's busy view: %+v", v)
	}
}

// What each row holds after a write: the ask goes to the row it changed; a
// row split off a series keeps the series' rooms (a retired one too); an
// instance alone can't change its series' rooms.
func TestAWritesRoomsGoToTheRowItChanged(t *testing.T) {
	ten := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	a, b := uuid.New(), uuid.New()
	series, tail := roomEvent("Standup", ten, 30), roomEvent("Standup, later", ten.AddDate(0, 0, 14), 30)
	held := map[uuid.UUID][]uuid.UUID{series.ID: {a}}
	asked := roomsAsk{rooms: []uuid.UUID{b}, set: true}

	plan, err := planHolds([]*models.Event{series}, []uuid.UUID{series.ID}, held, series.ID, asked)
	if err != nil || len(plan) != 1 || !sameRooms(plan[0].rooms, []uuid.UUID{b}) {
		t.Errorf("a whole series asked for b: %+v %v", plan, err)
	}
	plan, err = planHolds([]*models.Event{series, tail}, []uuid.UUID{series.ID, tail.ID}, held, series.ID, roomsAsk{})
	if err != nil || len(plan) != 2 || !sameRooms(plan[0].rooms, []uuid.UUID{a}) || !sameRooms(plan[1].rooms, []uuid.UUID{a}) || !sameRooms(plan[1].had, []uuid.UUID{a}) {
		t.Errorf("a split keeps the series' room on both: %+v %v", plan, err)
	}
	plan, _ = planHolds([]*models.Event{series, tail}, []uuid.UUID{series.ID, tail.ID}, held, series.ID, asked)
	if !sameRooms(plan[0].rooms, []uuid.UUID{a}) || !sameRooms(plan[1].rooms, []uuid.UUID{b}) {
		t.Errorf("a split asked for b gives b to the new row only: %+v", plan)
	}
	override := uuid.New()
	if _, err := planHolds([]*models.Event{series}, []uuid.UUID{override}, held, series.ID, asked); !errors.Is(err, errRoomsForSeries) {
		t.Errorf("an instance changing its series' rooms: %v", err)
	}
	same := roomsAsk{rooms: []uuid.UUID{a}, set: true}
	if plan, err := planHolds([]*models.Event{series}, []uuid.UUID{override}, held, series.ID, same); err != nil || len(plan) != 1 {
		t.Errorf("an instance naming its series' rooms: %+v %v", plan, err)
	}
	if plan, err := planHolds([]*models.Event{tail}, []uuid.UUID{tail.ID}, nil, uuid.Nil, roomsAsk{}); err != nil || len(plan) != 0 {
		t.Errorf("no rooms, nothing to hold: %+v %v", plan, err)
	}
	if !sameRooms([]uuid.UUID{a, b}, []uuid.UUID{b, a}) || sameRooms([]uuid.UUID{a}, []uuid.UUID{a, b}) || sameRooms([]uuid.UUID{a}, []uuid.UUID{b}) {
		t.Error("sameRooms")
	}
}

// Only a workspace member reaches the room directory or books a room. The
// session gate turns a guest or a bot away first (401); this is the refusal
// for whoever reaches the service past it (403).
func TestOnlyMembersReachTheRoomDirectory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	member, guest := uuid.New(), uuid.New()
	h.store.notMembers[guest] = true
	if err := h.s.membersOnlyRooms(ctx, member); err != nil {
		t.Errorf("a member was refused the rooms: %v", err)
	}
	if err := h.s.membersOnlyRooms(ctx, guest); !errors.Is(err, errRoomsForMembers) {
		t.Errorf("a guest reached the rooms: %v", err)
	}
}

// A write looks for clashes over its own span: a single event exactly
// [start, end), a finite series to its last end, and a series that started
// two years ago from a day before now, never a window wholly in the past.
func TestARoomCheckLooksOverTheEventsOwnSpan(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	ten := now.AddDate(0, 0, 2).Add(time.Hour)
	if w := checkWindow(roomEvent("One", ten, 30), now); !w.From.Equal(ten) || !w.To.Equal(ten.Add(30*time.Minute)) {
		t.Errorf("a single event is checked over %v to %v, want exactly its own span", w.From, w.To)
	}
	weekly := roomEvent("Weekly", ten, 30)
	weekly.RRule = "FREQ=WEEKLY;COUNT=4"
	until := ten.AddDate(0, 0, 21)
	weekly.UntilAt = &until
	if w := checkWindow(weekly, now); !w.From.Equal(ten) || !w.To.Equal(until.Add(30*time.Minute)) {
		t.Errorf("a finite series is checked over %v to %v, want to its last end", w.From, w.To)
	}
	old := roomEvent("Old", now.AddDate(-2, 0, 0), 30)
	old.RRule = "FREQ=WEEKLY"
	if w := checkWindow(old, now); !w.From.Equal(now.Add(-24*time.Hour)) || w.To.Sub(w.From) < 365*24*time.Hour {
		t.Errorf("a series begun two years ago is checked over %v to %v, want from a day before now for a year", w.From, w.To)
	}
}

// The rooms check's store loop (holdRoomsIn) is one pass per room-holding
// head: a batch as big as a Google sync page, three of its rows holding a
// room, plans three passes, none for the rows without one.
func TestRoomHoldsAreOnePassPerRoomHoldingHead(t *testing.T) {
	ten := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	held := map[uuid.UUID][]uuid.UUID{}
	var heads []*models.Event
	var wrote []uuid.UUID
	for i := 0; i < 250; i++ {
		e := roomEvent("Synced", ten.Add(time.Duration(i)*time.Hour), 30)
		if i%100 == 7 {
			held[e.ID] = []uuid.UUID{uuid.New()}
		}
		heads, wrote = append(heads, e), append(wrote, e.ID)
	}
	plan, err := planHolds(heads, wrote, held, uuid.Nil, roomsAsk{})
	if err != nil || len(plan) != len(held) {
		t.Fatalf("%d passes for a page with %d room-holding rows (%v), want one each", len(plan), len(held), err)
	}
	for _, h := range plan {
		if len(held[h.e.ID]) == 0 {
			t.Errorf("a pass for %s, which holds no room", h.e.ID)
		}
	}
}
