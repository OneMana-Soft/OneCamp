package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// gcal-15 head B: every OneCamp event, one-off or series, has a call room
// without anyone adding one, its own ("meet-<its id>") unless it holds a call
// already. A copy a calendar file or a provider's sync brought, and one read
// from an address, carry their own link; a booking hold and own time
// (focus, away, a working location, a task block, a reminder) have nobody to
// call. One given a guest link keeps that room whatever it is. Pure.
func TestEveryOneCampEventHasARoomOfItsOwn(t *testing.T) {
	id := uuid.New()
	own := "meet-" + id.String()
	for name, c := range map[string]struct {
		e    models.Event
		want string
	}{
		"a OneCamp event":              {models.Event{ID: id, Kind: string(EventPlain), Source: sourceOneCamp}, own},
		"one moved from the graph":     {models.Event{ID: id, Kind: string(EventPlain), Source: sourceMoved}, own},
		"one holding a call":           {models.Event{ID: id, Kind: string(EventPlain), Source: sourceOneCamp, CallRoom: "meet-older"}, "meet-older"},
		"a synced or imported copy":    {models.Event{ID: id, Kind: string(EventPlain), Source: sourceICS}, ""},
		"one read from an address":     {models.Event{ID: id, Kind: string(EventPlain), Source: sourceSubscribed}, ""},
		"an imported one given a link": {models.Event{ID: id, Kind: string(EventPlain), Source: sourceICS, CallRoom: "meet-x"}, "meet-x"},
		"a booking hold":               {models.Event{ID: id, Kind: string(EventBooking), Source: sourceOneCamp}, ""},
		"focus time":                   {models.Event{ID: id, Kind: string(EventFocus), Source: sourceOneCamp}, ""},
		"a reminder":                   {models.Event{ID: id, Kind: string(EventReminder), Source: sourceOneCamp}, ""},
	} {
		if got := roomOf(&c.e); got != c.want {
			t.Errorf("%s: room %q, want %q", name, got, c.want)
		}
	}
}

// readyRooms records the rooms made ready for their first join.
func (f *fakeCalls) readyRooms(t *testing.T) *[]string {
	t.Helper()
	var ready []string
	eventCalls.Ready = func(_ context.Context, event, _ uuid.UUID, _, room string) error {
		ready = append(ready, room)
		return nil
	}
	return &ready
}

// gcal-15 head B: the event's room is joined by whoever may read its details
// (its organiser, its guests, a reader of its calendar above busy-only), with
// guest access off: nothing is made when the event is, nobody is told, and its
// room is made ready only when someone joins, the same room every time. A
// busy-only reader, someone not on a private event who may only read its
// calendar, and someone not on it at all are told there is no such event; a
// copy a sync brought has no OneCamp room.
func TestAnEventsRoomIsJoinedByWhoeverMayReadItsDetails(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	ready := calls.readyRooms(t)
	jonas, maya, lee, rae := h.member("Jonas"), h.member("Maya"), h.member("Lee"), h.member("Rae")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end, Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	// Its invitation is told as ever; nothing of a call is.
	told := len(h.tells.tells)
	if calls.made != 0 || len(*ready) != 0 || len(h.tells.of(toldChanged)) != 0 {
		t.Fatalf("making the event made %d calls, readied %v, told %+v", calls.made, *ready, h.tells.of(toldChanged))
	}
	want := "meet-" + ev.ID
	for name, who := range map[string]*model.UserInfo{"its organiser": jonas, "its guest": maya} {
		j, err := h.s.JoinRoom(ctx, who, ev.ID, true)
		if err != nil || j.Room != want || j.Title != "Weekly sync" {
			t.Fatalf("%s joins: %+v, %v; want %s", name, j, err, want)
		}
	}
	if len(*ready) != 2 || (*ready)[0] != want || calls.made != 0 || len(h.tells.tells) != told {
		t.Fatalf("joining readied %v and made %d calls, told %d; want its one room readied, nothing else", *ready, calls.made, len(h.tells.tells)-told)
	}
	// Reading where to join makes nothing ready.
	if j, err := h.s.JoinRoom(ctx, maya, ev.ID, false); err != nil || j.Room != want || len(*ready) != 2 {
		t.Fatalf("a read of its room: %+v, %v, readied %v", j, err, *ready)
	}
	if _, err := h.s.JoinRoom(ctx, lee, ev.ID, true); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("someone not on it joins: %v, want no such event", err)
	}

	// A reader of Jonas's calendar: busy-only first, then with details; not a
	// private event of his unless she may change his calendar.
	cal, err := h.s.PersonCalendar(ctx, rae, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	private, err := h.s.Create(ctx, jonas, EventInput{Title: "Doctor", Start: start, End: end, Visibility: visibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.JoinRoom(ctx, rae, ev.ID, true); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a busy-only reader joins: %v, want no such event", err)
	}
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(rae).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	if j, err := h.s.JoinRoom(ctx, rae, ev.ID, true); err != nil || j.Room != want {
		t.Fatalf("a reader of its details joins: %+v, %v; want %s", j, err, want)
	}
	if _, err := h.s.JoinRoom(ctx, rae, private.ID, true); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a details reader joins a private event she isn't on: %v, want no such event", err)
	}
	if ok, err := h.s.mayJoinCallIn(ctx, rae, want, uuid.MustParse(ev.ID)); err != nil || !ok {
		t.Fatalf("on connect, a reader of its details may stay: %v, %v", ok, err)
	}
	if ok, err := h.s.mayJoinCallIn(ctx, lee, want, uuid.MustParse(ev.ID)); err != nil || ok {
		t.Fatalf("on connect, someone not on it may stay: %v, %v", ok, err)
	}

	// A copy a sync brought carries its own link.
	synced, err := h.s.Create(ctx, jonas, EventInput{Title: "From Google", Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	h.store.mu.Lock()
	h.store.events[uuid.MustParse(synced.ID)].Source = sourceICS
	h.store.mu.Unlock()
	if _, err := h.s.JoinRoom(ctx, jonas, synced.ID, true); !errors.Is(err, ErrNoEventCall) {
		t.Fatalf("a synced copy's room: %v, want none", err)
	}

	// Read, the event says it has a room and where a member joins it.
	got, err := h.s.Get(ctx, maya, ev.ID)
	if err != nil || got.Room == nil || got.Room.Join != RoomJoinPath+ev.ID {
		t.Fatalf("its read: %+v, %v", got.Room, err)
	}
	if seen, err := h.s.legacyGet(ctx, maya, ev.ID); err != nil || seen.Room == nil || seen.Room.Join != RoomJoinPath+ev.ID {
		t.Fatalf("its read on the /event routes: %+v, %v", seen, err)
	}
	if busy := busyView(*got); busy.Room != nil {
		t.Fatalf("a busy block says it has a room: %+v", busy.Room)
	}
	if s, _ := h.s.Get(ctx, jonas, synced.ID); s.Room != nil {
		t.Fatalf("a synced copy says it has a room: %+v", s.Room)
	}
}

// gcal-15 head B: an event that holds a call (a guest link added before) is
// joined in that room, and a guest link added later goes on the room members
// are in, so a call going on is never split.
func TestAGuestLinkGoesOnTheRoomMembersAreIn(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	calls.readyRooms(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Board prep", Start: start, End: end, Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := h.s.JoinRoom(ctx, maya, ev.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || call.Room != in.Room || len(calls.ended) != 0 {
		t.Fatalf("the guest link: %+v, %v, ended %v; want it on %s", call, err, calls.ended, in.Room)
	}
	if again, err := h.s.JoinRoom(ctx, maya, ev.ID, true); err != nil || again.Room != in.Room {
		t.Fatalf("joining with the link on: %+v, %v", again, err)
	}
	// Its link off (an admin's), a new one goes on the same room.
	delete(calls.live, call.Room)
	next, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || next.Room != in.Room || len(calls.ended) != 0 {
		t.Fatalf("a new link: %+v, %v, ended %v; want the same room, nothing ended", next, err, calls.ended)
	}
}
