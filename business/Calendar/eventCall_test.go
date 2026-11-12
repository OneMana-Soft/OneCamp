package Calendar

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// fakeCalls stands in for business/CallRoom's event calls: the calls made and
// the events they were made for (which alone end them or open them again),
// those still answering at their address, those ended (and, ended answering,
// kept to open again), those opened again, and how often a call's invitees
// were made to knock or let in again.
type fakeCalls struct {
	made     int
	events   []uuid.UUID
	madeFor  map[string]uuid.UUID
	live     map[string]*dgraphStruct.EventCall
	ended    []string
	closed   map[string]*dgraphStruct.EventCall
	reopened []string
	lobbied  int
}

func withFakeCalls(t *testing.T) *fakeCalls {
	t.Helper()
	f := &fakeCalls{madeFor: map[string]uuid.UUID{}, live: map[string]*dgraphStruct.EventCall{}, closed: map[string]*dgraphStruct.EventCall{}}
	was := eventCalls
	t.Cleanup(func() { eventCalls = was })
	OnEventCalls(EventCalls{
		// A link goes on the event's room when that room's call is the
		// event's own (business/CallRoom's adoption), else on the event's own
		// room: a new address each time.
		Make: func(_ context.Context, event, _ uuid.UUID, title, room string) (*dgraphStruct.EventCall, error) {
			f.made++
			f.events = append(f.events, event)
			if room == "" || f.madeFor[room] != event && room != "meet-"+event.String() {
				room = "meet-" + event.String()
			}
			slug := "call-" + uuid.NewString()[:8]
			c := &dgraphStruct.EventCall{Room: room, Slug: slug, Address: "/r/" + slug}
			f.live[room] = c
			f.madeFor[room] = event
			return c, nil
		},
		End: func(_ context.Context, event uuid.UUID, room string) error {
			if f.madeFor[room] != event {
				return nil
			}
			if c := f.live[room]; c != nil {
				f.closed[room] = c
			}
			delete(f.live, room)
			f.ended = append(f.ended, room)
			return nil
		},
		Reopen: func(_ context.Context, event uuid.UUID, room string) (bool, error) {
			if f.madeFor[room] != event {
				return false, nil
			}
			if c := f.closed[room]; c != nil {
				f.live[room] = c
				delete(f.closed, room)
				f.reopened = append(f.reopened, room)
			}
			return true, nil
		},
		Calls: func(_ context.Context, rooms []string) (map[string]*dgraphStruct.EventCall, error) {
			out := map[string]*dgraphStruct.EventCall{}
			for _, r := range rooms {
				if c := f.live[r]; c != nil {
					out[r] = c
				}
			}
			return out, nil
		},
		Lobby: func(_ context.Context, room string, inviteesWait bool) (bool, error) {
			f.lobbied++
			c := f.live[room]
			if c != nil {
				c.InviteesWait = inviteesWait
			}
			return c != nil, nil
		},
	})
	return f
}

// calendar B11 (calls zoom-01, meet-02): an event's organiser gives it a call
// on demand, once; its guests see the call where they see the event and join
// it, but a guest the organiser doesn't let change the event can neither add
// a call nor take it off. Taking it off, or deleting the event, ends it.
func TestAnEventsOwnCallIsItsOrganisersToGiveAndItsGuestsToJoin(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}

	// Maya, a guest who may not change it, may not give it a call.
	if _, err := h.s.AddCall(ctx, maya, ev.ID); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Fatalf("Maya gave Jonas's event a call: %v, want it refused", err)
	}
	if calls.made != 0 {
		t.Fatalf("a refused ask made %d calls", calls.made)
	}

	call, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || call == nil || call.Address == "" {
		t.Fatalf("Jonas gave his event a call: %+v, %v", call, err)
	}
	again, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || again.Room != call.Room || calls.made != 1 {
		t.Fatalf("asking again made another call (%d made): %+v, %v", calls.made, again, err)
	}
	if len(calls.events) != 1 || calls.events[0].String() != ev.ID {
		t.Fatalf("the call was made for %v, want the event %s, where what it makes lands", calls.events, ev.ID)
	}

	// Maya sees it where she sees the event, and still may not take it off.
	seen, err := h.s.legacyGet(ctx, maya, ev.ID)
	if err != nil || seen.Call == nil || seen.Call.Address != call.Address {
		t.Fatalf("Maya's read of the event: %+v, %v; want its call at %s", seen, err, call.Address)
	}
	if err := h.s.RemoveCall(ctx, maya, ev.ID); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Fatalf("Maya took the call off: %v, want it refused", err)
	}
	if len(calls.ended) != 0 {
		t.Fatalf("a refused ask ended %v", calls.ended)
	}

	// Once Jonas lets guests change it, she may.
	yes := true
	if _, err := h.s.Update(ctx, jonas, ev.ID, EventPatch{Sequence: seq(ev.Sequence + 1), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.RemoveCall(ctx, maya, ev.ID); err != nil {
		t.Fatalf("Maya, who may change it now, took the call off: %v", err)
	}
	if len(calls.ended) != 1 || calls.ended[0] != call.Room {
		t.Fatalf("taking it off ended %v, want %s", calls.ended, call.Room)
	}
	if seen, err := h.s.legacyGet(ctx, jonas, ev.ID); err != nil || seen.Call != nil {
		t.Fatalf("the event after its call was taken off: %+v, %v", seen.Call, err)
	}

	// Deleting the event ends the call it has.
	next, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := h.s.Get(ctx, jonas, ev.ID)
	if err := h.s.Delete(ctx, jonas, ev.ID, "", &cur.Sequence); err != nil {
		t.Fatal(err)
	}
	if calls.live[next.Room] != nil {
		t.Fatalf("the deleted event's call %s still answers", next.Room)
	}
}

// calendar V-641 (as Google's restored event keeps its Meet link): deleting
// an event closes its call, and bringing the event back from the trash opens
// that call again at the address its guests already have, with no new call
// made. A guest can't bring it back, so can't open its call again. A series'
// call closes and comes back with the series, whichever instance it was
// deleted from; an instance has its series' call, which deleting the
// instance alone never closed.
func TestARestoredEventsCallAnswersAtItsAddressAgain(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, jonas, ev.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls.live[call.Room] != nil {
		t.Fatalf("the deleted event's call %s still answers", call.Room)
	}

	// Maya, its guest, may not bring it back: its call stays closed.
	if _, err := h.s.Restore(ctx, maya, ev.ID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("Maya brought Jonas's event back: %v, want it refused", err)
	}
	if calls.live[call.Room] != nil || len(calls.reopened) != 0 {
		t.Fatalf("a refused restore opened the call again: %v", calls.reopened)
	}

	if _, err := h.s.Restore(ctx, jonas, ev.ID); err != nil {
		t.Fatal(err)
	}
	if calls.live[call.Room] == nil {
		t.Fatalf("the restored event's call %s doesn't answer", call.Room)
	}
	seen, err := h.s.legacyGet(ctx, maya, ev.ID)
	if err != nil || seen.Call == nil || seen.Call.Address != call.Address {
		t.Fatalf("Maya's read of the restored event: %+v, %v; want its call at %s", seen, err, call.Address)
	}
	if calls.made != 1 {
		t.Fatalf("%d calls made, want the one it had", calls.made)
	}

	// A series: one instance deleted and back never closes the series' call.
	lon := zone(t, "Europe/London")
	series := h.series(jonas, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	sc, err := h.s.AddCall(ctx, jonas, series.ID)
	if err != nil {
		t.Fatal(err)
	}
	var in []Event
	for _, e := range h.between(jonas, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)) {
		if e.SeriesID == series.ID {
			in = append(in, e)
		}
	}
	if len(in) != 3 {
		t.Fatalf("the series' instances: %+v", in)
	}
	if err := h.s.Delete(ctx, jonas, in[1].ID, ScopeThis, nil); err != nil {
		t.Fatal(err)
	}
	if calls.live[sc.Room] == nil {
		t.Fatalf("deleting one instance closed its series' call")
	}
	if _, err := h.s.Restore(ctx, jonas, in[1].ID); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.legacyGet(ctx, maya, in[1].ID); err != nil || got.Call == nil || got.Call.Address != sc.Address {
		t.Fatalf("the instance back: %+v, %v; want its series' call at %s", got, err, sc.Address)
	}

	// All events, from an instance: the series' call closes with it, and
	// opens again with it, for every instance.
	if err := h.s.Delete(ctx, jonas, in[2].ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	if calls.live[sc.Room] != nil {
		t.Fatalf("the deleted series' call %s still answers", sc.Room)
	}
	if _, err := h.s.Restore(ctx, jonas, series.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{series.ID, in[2].ID} {
		if got, err := h.s.legacyGet(ctx, maya, id); err != nil || got.Call == nil || got.Call.Address != sc.Address {
			t.Fatalf("%s back: %+v, %v; want the series' call at %s", id, got, err, sc.Address)
		}
	}
	if calls.made != 2 {
		t.Fatalf("%d calls made, want two, the event's and the series'", calls.made)
	}
}

// calendar V-641: a call its organiser took off on purpose stays off when the
// event, deleted after, comes back from the trash; so does one whose address
// stopped answering (an admin turned its link off) before the delete.
func TestACallTakenOffBeforeTheDeleteStaysOffWhenTheEventComesBack(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	for _, how := range []string{"taken off", "turned off"} {
		start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
		ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end,
			Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
		if err != nil {
			t.Fatal(err)
		}
		call, err := h.s.AddCall(ctx, jonas, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if how == "taken off" {
			if err := h.s.RemoveCall(ctx, jonas, ev.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			delete(calls.live, call.Room)
		}
		if err := h.s.Delete(ctx, jonas, ev.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.Restore(ctx, jonas, ev.ID); err != nil {
			t.Fatal(err)
		}
		if calls.live[call.Room] != nil {
			t.Errorf("%s: the call %s answers again", how, call.Room)
		}
		if seen, err := h.s.legacyGet(ctx, maya, ev.ID); err != nil || seen.Call != nil {
			t.Errorf("%s: Maya's read of the restored event: %+v, %v; want no call", how, seen.Call, err)
		}
	}
	if len(calls.reopened) != 0 {
		t.Errorf("calls opened again: %v", calls.reopened)
	}
}

// calendar V-641 (main, 6 Nov): "this and following" gives the new series its
// series' call, but the call is the series' that it was made for. A tail
// deleted with its instances and brought back comes back without it, offering
// to add one of its own, so nobody is pulled back into a call a deleted part
// of the series shared, even one its series took off since. Deleting a tail,
// or taking its call off, never ends its series' call. An instance its series
// no longer has comes back on its own without it too.
func TestASeriesTailNeverReopensNorEndsItsSeriesCall(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	instances := func(series *Event, want int) []Event {
		t.Helper()
		var in []Event
		for _, e := range h.between(jonas, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 11, 0, 0, 0, 0, lon)) {
			if e.SeriesID == series.ID {
				in = append(in, e)
			}
		}
		if len(in) != want {
			t.Fatalf("the series' instances: %+v, want %d", in, want)
		}
		return in
	}
	callOf := func(id string) *dgraphStruct.EventCall {
		t.Helper()
		seen, err := h.s.legacyGet(ctx, maya, id)
		if err != nil {
			t.Fatalf("Maya's read of %s: %v", id, err)
		}
		return seen.Call
	}
	roomOf := func(id string) string {
		t.Helper()
		row, err := h.s.store.get(ctx, uuid.MustParse(id))
		if err != nil || row == nil {
			t.Fatalf("%s: %+v %v", id, row, err)
		}
		return row.CallRoom
	}

	// Deleted "this and following", the series' call taken off, the tail back.
	series := h.series(jonas, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	sc, err := h.s.AddCall(ctx, jonas, series.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Delete(ctx, jonas, instances(series, 4)[2].ID, ScopeFollowing, nil); err != nil {
		t.Fatal(err)
	}
	if calls.live[sc.Room] == nil {
		t.Fatalf("deleting the series' later instances closed its call")
	}
	trash, err := h.s.Trash(ctx, jonas, nil)
	if err != nil || len(trash) != 1 {
		t.Fatalf("the trash: %+v %v", trash, err)
	}
	tail := trash[0].ID
	if err := h.s.RemoveCall(ctx, jonas, series.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Restore(ctx, jonas, tail); err != nil {
		t.Fatal(err)
	}
	if calls.live[sc.Room] != nil || len(calls.reopened) != 0 {
		t.Fatalf("the tail back opened the call its series took off: %v", calls.reopened)
	}
	if got := callOf(tail); got != nil || roomOf(tail) != "" {
		t.Fatalf("the tail back: %+v in %q, want no call", got, roomOf(tail))
	}
	own, err := h.s.AddCall(ctx, jonas, tail)
	if err != nil || own.Room == sc.Room || calls.live[sc.Room] != nil {
		t.Fatalf("the tail's own call: %+v, %v; the series' %s answers: %v", own, err, sc.Room, calls.live[sc.Room] != nil)
	}

	// Changed "this and following": the new series shares the call until it
	// is deleted, which leaves the series' call answering; back, it is without.
	other := h.series(jonas, "Retro", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	oc, err := h.s.AddCall(ctx, jonas, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	split := func(at Event, title string) *Event {
		t.Helper()
		p := patchAt(at, ScopeFollowing)
		p.Title = str(title)
		fresh, err := h.s.Update(ctx, jonas, at.ID, p)
		if err != nil || fresh.ID == other.ID {
			t.Fatalf("this and following: %+v, %v", fresh, err)
		}
		if got := callOf(fresh.ID); got == nil || got.Address != oc.Address {
			t.Fatalf("the new series: %+v, want its series' call at %s", got, oc.Address)
		}
		return fresh
	}
	later := split(instances(other, 4)[2], "Retro, room 2")
	if err := h.s.Delete(ctx, jonas, later.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls.live[oc.Room] == nil {
		t.Fatalf("deleting the new series ended its series' call")
	}
	if _, err := h.s.Restore(ctx, jonas, later.ID); err != nil {
		t.Fatal(err)
	}
	if got := callOf(later.ID); got != nil || roomOf(later.ID) != "" {
		t.Fatalf("the new series back: %+v in %q, want no call", got, roomOf(later.ID))
	}
	if got := callOf(other.ID); got == nil || got.Address != oc.Address {
		t.Fatalf("the series: %+v, want its call at %s", got, oc.Address)
	}

	// Its call taken off the new series alone: the series' call answers.
	sooner := split(instances(other, 2)[1], "Retro, room 3")
	if err := h.s.RemoveCall(ctx, jonas, sooner.ID); err != nil {
		t.Fatal(err)
	}
	if calls.live[oc.Room] == nil || callOf(sooner.ID) != nil {
		t.Fatalf("its call taken off the new series: the series' answers %v, the new one's %+v", calls.live[oc.Room] != nil, callOf(sooner.ID))
	}

	// An instance its series no longer has comes back on its own, without its
	// series' call.
	review := h.series(jonas, "Review", "FREQ=WEEKLY;BYDAY=TU", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	rc, err := h.s.AddCall(ctx, jonas, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved := instances(review, 4)[1]
	p := patchAt(moved, ScopeThis)
	p.Title = str("Review, offsite")
	if _, err := h.s.Update(ctx, jonas, moved.ID, p); err != nil {
		t.Fatal(err)
	}
	cur, err := h.s.Get(ctx, jonas, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Update(ctx, jonas, review.ID, EventPatch{Sequence: seq(cur.Sequence), Recurrence: &Recurrence{Rule: "FREQ=WEEKLY;BYDAY=WE"}}); err != nil {
		t.Fatal(err)
	}
	if trash, err = h.s.Trash(ctx, jonas, nil); err != nil || len(trash) != 1 {
		t.Fatalf("the trash: %+v %v", trash, err)
	}
	back, err := h.s.Restore(ctx, jonas, trash[0].ID)
	if err != nil || back.SeriesID != "" {
		t.Fatalf("the instance back on its own: %+v, %v", back, err)
	}
	if got := callOf(back.ID); got != nil || roomOf(back.ID) != "" || calls.live[rc.Room] == nil {
		t.Fatalf("the instance back on its own: %+v in %q; its series' call answers %v", got, roomOf(back.ID), calls.live[rc.Room] != nil)
	}
}

// meet-02 (Meet's quick access off): whoever may change an event makes its
// invitees knock at its call, and its guests read that where they read the
// event. A guest who may not change it can't, and an event without a call
// says it has none.
func TestOnlyWhoMayChangeAnEventMakesItsInviteesKnock(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetCallLobby(ctx, jonas, ev.ID, true); !errors.Is(err, ErrNoEventCall) {
		t.Fatalf("Jonas made the invitees of an event without a call knock: %v, want ErrNoEventCall", err)
	}
	if _, err := h.s.AddCall(ctx, jonas, ev.ID); err != nil {
		t.Fatal(err)
	}

	// Maya, a guest who may not change it, may not make its invitees knock.
	if _, err := h.s.SetCallLobby(ctx, maya, ev.ID, true); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Fatalf("Maya made Jonas's invitees knock: %v, want it refused", err)
	}
	if calls.lobbied != 0 {
		t.Fatalf("a refused ask changed the call's lobby %d times", calls.lobbied)
	}

	call, err := h.s.SetCallLobby(ctx, jonas, ev.ID, true)
	if err != nil || call == nil || !call.InviteesWait {
		t.Fatalf("Jonas made his invitees knock: %+v, %v; want the call saying they knock", call, err)
	}
	if seen, err := h.s.legacyGet(ctx, maya, ev.ID); err != nil || seen.Call == nil || !seen.Call.InviteesWait {
		t.Fatalf("Maya's read of the event: %+v, %v; want its call saying invitees knock", seen, err)
	}
	if call, err := h.s.SetCallLobby(ctx, jonas, ev.ID, false); err != nil || call == nil || call.InviteesWait {
		t.Fatalf("Jonas let his invitees in again: %+v, %v", call, err)
	}
}

// A call whose address stopped answering (an admin turned its link off) isn't
// shown, and asking for one gives the same room a new link. Two asks
// at once make one call: the one that loses ends its own and answers the
// winner's.
func TestAnEventsCallThatStoppedAnsweringIsReplacedAndARaceMakesOne(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	jonas := h.member("Jonas")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Board prep", Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	delete(calls.live, first.Room) // its link turned off
	if seen, _ := h.s.legacyGet(ctx, jonas, ev.ID); seen.Call != nil {
		t.Fatalf("a call that doesn't answer was shown: %+v", seen.Call)
	}
	// Asking again gives its room a new link (gcal-15 head B): the same
	// room, so a call going on there isn't split, and nothing ended.
	second, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || second.Room != first.Room || second.Slug == first.Slug {
		t.Fatalf("asking again: %+v, %v; want a new link on %s", second, err, first.Room)
	}
	if len(calls.ended) != 0 {
		t.Fatalf("a new link ended %v", calls.ended)
	}

	// Someone else's ask won while this one made its call.
	id, _ := uuid.Parse(ev.ID)
	h.store.mu.Lock()
	h.store.events[id].CallRoom = "" // this ask read the event before the other's call
	h.store.mu.Unlock()
	winner := "meet-" + uuid.NewString()
	calls.live[winner] = &dgraphStruct.EventCall{Room: winner, Slug: "won", Address: "/r/won"}
	race := eventCalls.Make
	eventCalls.Make = func(ctx context.Context, e, o uuid.UUID, title, room string) (*dgraphStruct.EventCall, error) {
		c, err := race(ctx, e, o, title, room)
		h.store.mu.Lock()
		h.store.events[id].CallRoom = winner
		h.store.mu.Unlock()
		return c, err
	}
	got, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || got.Room != winner {
		t.Fatalf("the losing ask answered %+v, %v; want the winner's %s", got, err, winner)
	}
	if last := calls.ended[len(calls.ended)-1]; last == winner || calls.live[last] != nil {
		t.Fatalf("the losing ask's own call wasn't ended: %v", calls.ended)
	}
}

// meet-02 (as Google Meet's calendar invitees): an event's guests by email are
// sent its call. Add call sends them the updated invitation, whose email's Join
// call carries their own key (the one their answer link has) in the fragment,
// and whose calendar file has the call's plain link, never the key. Remove call
// sends it again without the call. Members hear of it live, so the change tells
// only guests by email; an event with none tells nobody.
func TestAnEventsGuestsByEmailAreSentItsCallWithTheirOwnKey(t *testing.T) {
	h := newHarness(t)
	withFakeCalls(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	h.people[sam.UserDgraphInfo.Uuid].EmailID = "sam@acme.example"
	mail := captureMail(t, true)
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>"}, maya)

	call, err := h.s.AddCall(ctx, sam, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	told := h.tells.of(toldChanged)
	if len(told) != 1 || len(told[0].changed) != 1 || told[0].changed[0] != "call" || !told[0].outsidersOnly {
		t.Fatalf("Add call told %+v, want one change of the call, to guests by email only", told)
	}
	h.s.tellOutsiders(ctx, told[0], true)
	if len(*mail) != 1 {
		t.Fatalf("%d emails", len(*mail))
	}
	m := (*mail)[0]
	if m.to != "alex@example.org" || !strings.HasPrefix(m.subject, "Updated invitation: Intro call") || !strings.Contains(m.html, "Sam added a call to Intro call") {
		t.Errorf("mail to %q, %q, headline not the call's:\n%s", m.to, m.subject, m.html)
	}
	at := strings.Index(m.html, "/rsvp/")
	if at < 0 {
		t.Fatalf("no answer link:\n%s", m.html)
	}
	key := m.html[at+len("/rsvp/"):]
	key = key[:strings.IndexAny(key, "?\"")]
	plain := callURL(call.Address, "")
	if join := html.EscapeString(callURL(call.Address, key)); !strings.Contains(m.html, `href="`+join+`"><strong>Join the call</strong></a>`) {
		t.Errorf("no Join call to %q with Alex's own key:\n%s", join, m.html)
	}
	if !strings.Contains(m.ics, "CONFERENCE;VALUE=URI;FEATURE=AUDIO,VIDEO:"+plain) || !strings.Contains(m.ics, "Join the call: "+plain) {
		t.Errorf("the file has no call link %q:\n%s", plain, m.ics)
	}
	if strings.Contains(m.ics, "#invitation") || strings.Contains(m.ics, key) {
		t.Errorf("the file carries the key:\n%s", m.ics)
	}

	// Remove call: the invitation again, without the call.
	*mail = nil
	if err := h.s.RemoveCall(ctx, sam, e.ID); err != nil {
		t.Fatal(err)
	}
	told = h.tells.of(toldChanged)
	if len(told) != 2 {
		t.Fatalf("Remove call told %d changes in all, want 2", len(told))
	}
	h.s.tellOutsiders(ctx, told[1], true)
	if len(*mail) != 1 || !strings.Contains((*mail)[0].html, "Sam removed the call from Intro call") || strings.Contains((*mail)[0].html, "Join the call") || strings.Contains((*mail)[0].ics, "CONFERENCE") || strings.Contains((*mail)[0].ics, call.Address) {
		t.Errorf("after Remove call: %+v", *mail)
	}

	// Members only: nobody to email, nothing told.
	only := h.withEmails(sam, "Team only", nil, maya)
	if _, err := h.s.AddCall(ctx, sam, only.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.tells.of(toldChanged)); n != 2 {
		t.Errorf("a call on a members-only event told %d changes in all, want still 2", n)
	}

	// Over: nobody joins it now, so a call added to it mails nobody.
	start, end := timed(time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC), 45, "Europe/London")
	past, err := h.s.Create(ctx, sam, EventInput{Title: "Last week", Start: start, End: end, Attendees: []GuestInput{{Email: "alex@example.org"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.AddCall(ctx, sam, past.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(h.tells.of(toldChanged)); n != 2 {
		t.Errorf("a call on an event that is over told %d changes in all, want still 2", n)
	}
}
