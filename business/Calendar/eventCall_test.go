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

// fakeCalls stands in for business/CallRoom's event calls: the calls made,
// those still answering at their address, and those ended.
type fakeCalls struct {
	made  int
	live  map[string]*dgraphStruct.EventCall
	ended []string
}

func withFakeCalls(t *testing.T) *fakeCalls {
	t.Helper()
	f := &fakeCalls{live: map[string]*dgraphStruct.EventCall{}}
	was := eventCalls
	t.Cleanup(func() { eventCalls = was })
	OnEventCalls(EventCalls{
		Make: func(_ context.Context, _ uuid.UUID, title string) (*dgraphStruct.EventCall, error) {
			f.made++
			room := "meet-" + uuid.NewString()
			c := &dgraphStruct.EventCall{Room: room, Slug: "call-" + room[5:13], Address: "/r/call-" + room[5:13]}
			f.live[room] = c
			return c, nil
		},
		End: func(_ context.Context, room string) error {
			delete(f.live, room)
			f.ended = append(f.ended, room)
			return nil
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

// A call whose address stopped answering (an admin turned its link off) isn't
// shown, and asking for one makes a new call and ends the dead one. Two asks
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
	second, err := h.s.AddCall(ctx, jonas, ev.ID)
	if err != nil || second.Room == first.Room {
		t.Fatalf("asking again: %+v, %v; want a new call", second, err)
	}
	if len(calls.ended) != 1 || calls.ended[0] != first.Room {
		t.Fatalf("the dead call wasn't ended: %v", calls.ended)
	}

	// Someone else's ask won while this one made its call.
	id, _ := uuid.Parse(ev.ID)
	h.store.mu.Lock()
	h.store.events[id].CallRoom = "" // this ask read the event before the other's call
	h.store.mu.Unlock()
	winner := "meet-" + uuid.NewString()
	calls.live[winner] = &dgraphStruct.EventCall{Room: winner, Slug: "won", Address: "/r/won"}
	race := eventCalls.Make
	eventCalls.Make = func(ctx context.Context, o uuid.UUID, title string) (*dgraphStruct.EventCall, error) {
		c, err := race(ctx, o, title)
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
