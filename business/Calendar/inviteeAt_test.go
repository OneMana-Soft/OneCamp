package Calendar

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// meet-02 (1): an event's invitee skips its call's lobby by the key their
// invitation's link already carries. It counts only at that event's own call,
// and only while the invitation's page would open: never at another event's
// call, never for a key that names nobody, never long after the meeting or
// once the event is gone.
func TestAnInviteeIsKnownAtTheirOwnEventsCallOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>"})
	other := h.withEmails(sam, "Board review", []string{"jo@example.org"})
	id := uuid.MustParse(e.ID)
	h.store.events[id].CallRoom = "meet-intro"
	h.store.events[uuid.MustParse(other.ID)].CallRoom = "meet-board"
	alex := answerOf(h.store.guests[id], models.GuestKey{Email: "alex@example.org"})
	raw, err := helpers.DecryptSecret(alex.LinkSealed)
	if err != nil || raw == "" {
		t.Fatalf("Alex's key: %q %v", raw, err)
	}
	for _, c := range []struct {
		name, secret, room string
		at                 time.Time
		want               bool
	}{
		{"Alex at the intro's call", raw, "meet-intro", h.clock, true},
		{"Alex at another event's call", raw, "meet-board", h.clock, false},
		{"Alex at no call", raw, "", h.clock, false},
		{"no key", "", "meet-intro", h.clock, false},
		{"a key that names nobody", "nope", "meet-intro", h.clock, false},
		{"Alex long after the meeting", raw, "meet-intro", h.clock.Add(60 * 24 * time.Hour), false},
	} {
		if got, err := h.s.inviteeAt(ctx, c.secret, c.room, c.at); err != nil || (got != nil) != c.want {
			t.Errorf("%s: invited %+v (%v), want %v", c.name, got, err, c.want)
		}
	}
	// Whose invitation it was, for the join's audit row and the name shown.
	if got, _ := h.s.inviteeAt(ctx, raw, "meet-intro", h.clock); got == nil || got.EventID != e.ID || got.Email != "alex@example.org" || got.Name != "Alex Kim" {
		t.Errorf("Alex's invitation: %+v", got)
	}
	jo := answerOf(h.store.guests[uuid.MustParse(other.ID)], models.GuestKey{Email: "jo@example.org"})
	joRaw, _ := helpers.DecryptSecret(jo.LinkSealed)
	if got, _ := h.s.inviteeAt(ctx, joRaw, "meet-board", h.clock); got == nil || got.Name != "jo" {
		t.Errorf("Jo's invitation, no name on it: %+v, want her address's first part", got)
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.inviteeAt(ctx, raw, "meet-intro", h.clock); err != nil || got != nil {
		t.Errorf("Alex once the event is deleted: invited %+v (%v), want none", got, err)
	}
}

// meet-02 (1): a guest's page offers the event's own call while its address
// answers and the event hasn't ended, so the guest joins from it with their
// invitation. An event without a live call, or one that has ended, offers none.
func TestAGuestsPageOffersTheEventsCallUntilItEnds(t *testing.T) {
	h := newHarness(t)
	calls := withFakeCalls(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>"})
	id := uuid.MustParse(e.ID)
	alex := answerOf(h.store.guests[id], models.GuestKey{Email: "alex@example.org"})
	raw, _ := helpers.DecryptSecret(alex.LinkSealed)
	if v, err := h.s.invitation(ctx, raw, h.clock); err != nil || v.CallAddress != "" {
		t.Fatalf("no call yet: %q %v", v.CallAddress, err)
	}
	h.store.events[id].CallRoom = "meet-intro"
	if v, err := h.s.invitation(ctx, raw, h.clock); err != nil || v.CallAddress != "" {
		t.Errorf("a call whose address doesn't answer: %q %v", v.CallAddress, err)
	}
	calls.live["meet-intro"] = &dgraphStruct.EventCall{Room: "meet-intro", Slug: "intro-call-1a2b3c4d", Address: "/r/intro-call-1a2b3c4d"}
	if v, err := h.s.invitation(ctx, raw, h.clock); err != nil || v.CallAddress != "/r/intro-call-1a2b3c4d" {
		t.Errorf("the event's live call: %q %v, want its address", v.CallAddress, err)
	}
	end := lastEnd(h.store.events[id])
	if v, err := h.s.invitation(ctx, raw, end.Add(time.Minute)); err != nil || !v.Ended || v.CallAddress != "" {
		t.Errorf("after the event ended: %+v %v, want ended and no call", v, err)
	}
}
