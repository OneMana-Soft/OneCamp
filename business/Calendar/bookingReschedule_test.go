package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

// A guest's link moves only a booking still to come, not cancelled, with its
// meeting on the owner's calendar. Every other link answers as a missing one
// (ErrNoPage), before any page or slot work, so a link can't be told apart
// by how it fails: unknown, cancelled, ended or begun all read the same.
func TestMovableBookingsAreOnlyLiveOnesStillToCome(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	event := uuid.New()
	cancelled := now.Add(-time.Hour)
	for name, tc := range map[string]struct {
		b    *bookingModel.Booking
		want bool
	}{
		"no booking":       {nil, false},
		"still to come":    {&bookingModel.Booking{EventUUID: &event, StartsAt: now.Add(time.Hour)}, true},
		"cancelled":        {&bookingModel.Booking{EventUUID: &event, StartsAt: now.Add(time.Hour), CancelledAt: &cancelled}, false},
		"begun":            {&bookingModel.Booking{EventUUID: &event, StartsAt: now.Add(-time.Minute)}, false},
		"ended":            {&bookingModel.Booking{EventUUID: &event, StartsAt: now.Add(-2 * time.Hour)}, false},
		"starts right now": {&bookingModel.Booking{EventUUID: &event, StartsAt: now}, false},
		"no meeting made":  {&bookingModel.Booking{StartsAt: now.Add(time.Hour)}, false},
	} {
		if got := movable(tc.b, now); got != tc.want {
			t.Errorf("%s: movable %v, want %v", name, got, tc.want)
		}
	}
}

// A move's slot check reads the owner's busy times with the booking's own
// meeting left out, and only it: another meeting still blocks its time, and
// the time the booking holds now is free to move within.
func TestMoveSlotCheckIgnoresOnlyItsOwnEvent(t *testing.T) {
	owner := uuid.New()
	hours := WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"}
	loc, err := hours.Check()
	if err != nil {
		t.Fatal(err)
	}
	l := &livePage{page: &bookingModel.Page{Id: uuid.New(), UserId: owner, DurationMinutes: 30, MaxDaysAhead: 30, Active: true},
		hours: hours, loc: loc}
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 21, h, m, 0, 0, time.UTC) }
	own := &dgraphStruct.DgraphEvent{Uuid: uuid.NewString()}
	// The booking holds 10:00-11:00; another meeting holds 13:00.
	old := readBusy
	t.Cleanup(func() { readBusy = old })
	var asked []*dgraphStruct.DgraphEvent
	readBusy = func(_ context.Context, who string, _, _ time.Time, opts BusyOptions) (Busy, error) {
		if who != owner.String() {
			t.Errorf("busy read for %s, not the page's owner", who)
		}
		asked = append(asked, opts.Except)
		busy := []Interval{{Start: at(13, 0), End: at(13, 30)}}
		if opts.Except != own {
			busy = append([]Interval{{Start: at(10, 0), End: at(11, 0)}}, busy...)
		}
		return Busy{Intervals: busy}, nil
	}
	ctx := context.Background()
	if err := l.slotFree(ctx, at(10, 30), at(11, 0), now, own); err != nil {
		t.Errorf("moving within the booking's own time: %v", err)
	}
	var ae *AvailabilityError
	if err := l.slotFree(ctx, at(13, 0), at(13, 30), now, own); !errors.As(err, &ae) {
		t.Errorf("moving onto another meeting: %v, want refused", err)
	}
	if err := l.slotFree(ctx, at(10, 30), at(11, 0), now, nil); !errors.As(err, &ae) {
		t.Errorf("a new booking onto the booked time: %v, want refused", err)
	}
	for i, e := range asked[:2] {
		if e != own {
			t.Errorf("read %d left out %v, want the booking's own event", i, e)
		}
	}
	if asked[2] != nil {
		t.Errorf("a new booking's read left out %v", asked[2])
	}
}

// A move is refused like a booking would be: out of the page's hours, too
// soon (notice, or the past), past its horizon, or onto a busy time.
func TestMoveRefusalsFollowThePagesRules(t *testing.T) {
	owner := uuid.New()
	hours := WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"}
	loc, err := hours.Check()
	if err != nil {
		t.Fatal(err)
	}
	l := &livePage{page: &bookingModel.Page{Id: uuid.New(), UserId: owner, DurationMinutes: 30, MaxDaysAhead: 7,
		MinNoticeMinutes: 120, Active: true}, hours: hours, loc: loc}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	old := readBusy
	t.Cleanup(func() { readBusy = old })
	readBusy = func(context.Context, string, time.Time, time.Time, BusyOptions) (Busy, error) {
		return Busy{Intervals: []Interval{{Start: now.Add(26 * time.Hour), End: now.Add(27 * time.Hour)}}}, nil
	}
	ctx := context.Background()
	check := func(start time.Time) error {
		s, e, err := l.slotTimes(start, now)
		if err != nil {
			return err
		}
		return l.slotFree(ctx, s, e, now, &dgraphStruct.DgraphEvent{Uuid: uuid.NewString()})
	}
	if err := check(now.Add(24 * time.Hour)); err != nil {
		t.Fatalf("a free slot tomorrow: %v", err)
	}
	for name, start := range map[string]time.Time{
		"out of hours":      now.Add(24*time.Hour + 9*time.Hour),
		"within the notice": now.Add(time.Hour),
		"in the past":       now.Add(-24 * time.Hour),
		"past the horizon":  now.Add(8 * 24 * time.Hour),
		"onto a busy time":  now.Add(26 * time.Hour),
		"off the slot grid": now.Add(24*time.Hour + 7*time.Minute),
	} {
		var ae *AvailabilityError
		if err := check(start); !errors.As(err, &ae) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	// A move keeps the page's length, whatever the guest sends.
	if s, e, err := l.slotTimes(now.Add(24*time.Hour+30*time.Second), now); err != nil || !s.Equal(now.Add(24*time.Hour)) ||
		e.Sub(s) != 30*time.Minute {
		t.Errorf("slot times %v..%v %v", s, e, err)
	}
}

// A guest's move's page is the page of its own booking alone: a booking of
// another page changed under the move reads its own page, as an owner's
// change does (CR-394 round 2, V-1203).
func TestGuestMovePageIsOnlyItsOwnBookings(t *testing.T) {
	p := &bookingModel.Page{Id: uuid.New()}
	ctx := context.WithValue(context.Background(), guestMoveKey{}, p)
	if own, other := guestMoveOf(ctx, &bookingModel.Booking{PageId: p.Id}), guestMoveOf(ctx, &bookingModel.Booking{PageId: uuid.New()}); own != p || other != nil {
		t.Errorf("the move's page for its own booking: %v (want its page), for another page's booking: %v (want none)", own, other)
	}
}

// V-1194 N4, N8: a time inside the page's notice, or past, is refused by
// slotTimes, the one check a booking and a move share, so both refuse it
// before the owner's calendars are read. The notice's own edge is taken.
func TestATimeInsideTheNoticeIsRefusedBeforeTheCalendars(t *testing.T) {
	hours := WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"}
	loc, err := hours.Check()
	if err != nil {
		t.Fatal(err)
	}
	l := &livePage{page: &bookingModel.Page{Id: uuid.New(), UserId: uuid.New(), DurationMinutes: 30, MaxDaysAhead: 7,
		MinNoticeMinutes: 120, Active: true}, hours: hours, loc: loc}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	edge := now.Add(2 * time.Hour)
	if s, e, err := l.slotTimes(edge, now); err != nil || !s.Equal(edge) || !e.Equal(edge.Add(30*time.Minute)) {
		t.Errorf("the notice's edge: %v..%v %v, want taken", s, e, err)
	}
	// The guest reads why: a time gone by says so, not "too soon".
	const passed, tooSoon = "That time has passed. Pick another.", "That time is too soon. Pick a later one."
	refused := func(name string, start time.Time, why string) {
		t.Helper()
		var ae *AvailabilityError
		if _, _, err := l.slotTimes(start, now); !errors.As(err, &ae) || ae.Msg != why {
			t.Errorf("%s: %v, want refused with %q", name, err, why)
		}
	}
	refused("a minute inside the notice", edge.Add(-time.Minute), tooSoon)
	refused("now, with two hours' notice", now, tooSoon)
	refused("in the past", now.Add(-24*time.Hour), passed)
	// No notice: now is taken, a minute ago never.
	l.page.MinNoticeMinutes = 0
	if _, _, err := l.slotTimes(now, now); err != nil {
		t.Errorf("now with no notice: %v, want taken", err)
	}
	refused("a minute ago with no notice", now.Add(-time.Minute), passed)
}
