package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// StatusFor reads the person's events at the moment, as the busy engine
// reads them, and says time off before focus time, until the last of its
// kind ends; an event's title never leaves.
func TestStatusForIsTimeOffThenFocus(t *testing.T) {
	yes, no := true, false
	gone := at(1, 0)
	ev := func(s, e time.Time, away, focus bool) *dgraphStruct.DgraphEvent {
		return &dgraphStruct.DgraphEvent{Title: "Interview with Priya", StartTime: &s, EndTime: &e, IsAway: &away, IsFocus: &focus,
			CreatedBy: &dgraphStruct.DgraphUser{Uuid: "maya"}}
	}
	var asked string
	var from, to time.Time
	reading := func(events ...*dgraphStruct.DgraphEvent) {
		prev := eventsOf
		t.Cleanup(func() { eventsOf = prev })
		eventsOf = func(_ context.Context, who string, f, e *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
			asked, from, to = who, *f, *e
			return events, nil
		}
	}
	now := at(10, 30)

	reading(ev(at(10, 0), at(11, 0), no, yes), ev(at(9, 0), at(12, 0), no, yes), ev(at(10, 0), at(11, 0), no, no))
	s, err := Statuses.StatusFor(context.Background(), "maya", now)
	if err != nil || s.Kind != StatusFocus || s.Until == nil || !s.Until.Equal(at(12, 0)) {
		t.Errorf("focus: %+v %v", s, err)
	}
	if asked != "maya" || !from.Equal(now) || !to.Equal(now) {
		t.Errorf("read %q from %v to %v, want maya at %v", asked, from, to, now)
	}

	reading(ev(at(9, 0), at(12, 0), no, yes), ev(at(0, 0), at(23, 0), yes, no))
	if s, _ := Statuses.StatusFor(context.Background(), "maya", now); s.Kind != StatusAway || !s.Until.Equal(at(23, 0)) {
		t.Errorf("time off wins: %+v", s)
	}

	ended, deleted, later := ev(at(9, 0), now, no, yes), ev(at(9, 0), at(12, 0), yes, no), ev(at(11, 0), at(12, 0), yes, no)
	deleted.DeletedAt = &gone
	reading(ended, deleted, later)
	if s, _ := Statuses.StatusFor(context.Background(), "maya", now); s.Kind != "" || s.Until != nil {
		t.Errorf("an ended, a deleted and a later event say nothing: %+v", s)
	}
	// An ordinary event running is a meeting (calendar A3), until it ends; never its title.
	reading(ended, deleted, later, ev(at(10, 0), at(11, 0), no, no))
	if s, _ := Statuses.StatusFor(context.Background(), "maya", now); s.Kind != StatusBusy || s.Until == nil || !s.Until.Equal(at(11, 0)) {
		t.Errorf("a meeting: %+v", s)
	}
}

// A person's status is their own time off and focus time, as the push gate
// and the workload count them: the events they made. Being invited to a
// teammate's holiday or focus block says nothing of them.
func TestStatusForCountsOnlyThePersonsOwnEvents(t *testing.T) {
	yes, no := true, false
	start, end := at(9, 0), at(12, 0)
	by := func(maker string, away, focus bool) *dgraphStruct.DgraphEvent {
		return &dgraphStruct.DgraphEvent{Title: "Holiday", StartTime: &start, EndTime: &end, IsAway: &away, IsFocus: &focus,
			CreatedBy:    &dgraphStruct.DgraphUser{Uuid: maker},
			Participants: []*dgraphStruct.DgraphUser{{Uuid: "maya"}, {Uuid: "jonas"}}}
	}
	prev := eventsOf
	t.Cleanup(func() { eventsOf = prev })
	events := []*dgraphStruct.DgraphEvent{by("jonas", yes, no), by("jonas", no, yes)}
	eventsOf = func(context.Context, string, *time.Time, *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		return events, nil
	}
	if s, err := Statuses.StatusFor(context.Background(), "maya", at(10, 30)); err != nil || s.Kind != "" || s.Until != nil {
		t.Errorf("a guest of Jonas's time off and focus time is %+v (%v), want nothing", s, err)
	}
	if s, _ := Statuses.StatusFor(context.Background(), "JONAS", at(10, 30)); s.Kind != StatusAway {
		t.Errorf("Jonas's own time off says %+v", s)
	}
	events = append(events, by("maya", no, yes))
	if s, _ := Statuses.StatusFor(context.Background(), "maya", at(10, 30)); s.Kind != StatusFocus || !s.Until.Equal(end) {
		t.Errorf("Maya's own focus time says %+v", s)
	}
}

// Events is the calendar in its one store. On a server that hasn't switched
// the store on, every call says so (a 503 on the API) rather than answer from
// a store nothing writes to yet.
func TestEventsIsTheOneStore(t *testing.T) {
	if Events != Service(stored) {
		t.Fatal("Events isn't the one store's service")
	}
	t.Setenv("CALENDAR_STORE", "graph")
	ctx, who, id := context.Background(), &model.UserInfo{}, uuid.NewString()
	if _, err := Events.Get(ctx, who, id); !errors.Is(err, ErrStoreOff) {
		t.Errorf("Get: %v", err)
	}
	if _, _, err := Events.Range(ctx, who, at(9, 0), at(10, 0)); !errors.Is(err, ErrStoreOff) {
		t.Errorf("Range: %v", err)
	}
	if err := Events.Respond(ctx, who, id, Declined); !errors.Is(err, ErrStoreOff) {
		t.Errorf("Respond: %v", err)
	}
}

// The calendar's kinds are registered with the Access Resolver: a second
// rule for either panics where it is registered.
func TestTheCalendarKindsAreRegistered(t *testing.T) {
	for _, kind := range []string{KindEvent, KindBooking} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s isn't registered with the Access Resolver", kind)
				}
			}()
			accessBusiness.RegisterKind(kind, eventRules{})
		}()
	}
}

// An event is its creator's to manage and its guests' to see, as GetEvent,
// UpdateEvent and DeleteEvent decide; nobody else's.
func TestAnEventDecidesAsTodaysRules(t *testing.T) {
	ev := &dgraphStruct.DgraphEvent{
		CreatedBy:    &dgraphStruct.DgraphUser{Uuid: "maya"},
		Participants: []*dgraphStruct.DgraphUser{{Uuid: "jonas"}, nil},
	}
	for who, want := range map[string]accessBusiness.Level{
		"maya": accessBusiness.LevelFull, "jonas": accessBusiness.LevelView, "sam": accessBusiness.LevelNone, "": accessBusiness.LevelNone,
	} {
		if d := eventDecision(ev, who); d.Level != want || d.Owner != (who == "maya") {
			t.Errorf("%q: %+v, want %s", who, d, want)
		}
	}
}

// A booking page is its owner's to manage and, while it is live, anyone's to
// see; a deleted one is nobody's.
func TestABookingPageDecidesAsTodaysRules(t *testing.T) {
	owner := uuid.New()
	gone := time.Now()
	live := &bookingModel.Page{UserId: owner, Active: true}
	off := &bookingModel.Page{UserId: owner}
	deleted := &bookingModel.Page{UserId: owner, Active: true, DeletedAt: &gone}
	for _, c := range []struct {
		name string
		p    *bookingModel.Page
		who  string
		want accessBusiness.Level
	}{
		{"its owner", live, owner.String(), accessBusiness.LevelFull},
		{"its owner, switched off", off, owner.String(), accessBusiness.LevelFull},
		{"anyone, live", live, uuid.NewString(), accessBusiness.LevelView},
		{"a visitor not signed in, live", live, "", accessBusiness.LevelView},
		{"anyone, switched off", off, "", accessBusiness.LevelNone},
		{"anyone, deleted", deleted, "", accessBusiness.LevelNone},
		{"its owner, deleted", deleted, owner.String(), accessBusiness.LevelNone},
	} {
		if d := pageDecision(c.p, c.who); d.Level != c.want {
			t.Errorf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
	if d := pageDecision(deleted, owner.String()); !d.Deleted || d.Was != accessBusiness.LevelFull {
		t.Errorf("a deleted page keeps what its owner had: %+v", d)
	}
}
