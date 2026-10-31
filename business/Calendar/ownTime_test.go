package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	"github.com/google/uuid"
)

// Own time (ownTimeKinds: focus time, an out of office, a working location, a
// reminder, a task block) is its owner's alone, through every writer: the
// calendar's own create and change, and the /event routes' create and update.
// A write that adds a guest is refused in the kind's own words, and so is a
// change that makes a meeting with guests own time; naming oneself adds no
// one; and an older one that has guests already can still be changed.
func TestOwnTimeTakesNoGuestsThroughAnyWriter(t *testing.T) {
	h := newHarness(t)
	stubTasks(t)
	ctx := context.Background()
	maya, lee := withGraphUID(h.member("Maya"), "0xmaya"), h.member("Lee")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	from, to := start.At.Format(time.RFC3339), end.At.Format(time.RFC3339)
	guests, herself := []GuestInput{{UUID: lee.UserDgraphInfo.Uuid}}, []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}
	refused := func(what string, err error, want string) {
		t.Helper()
		var in *EventInputError
		if !errors.As(err, &in) || in.Error() != want {
			t.Errorf("%s: %v, want %q", what, err, want)
		}
	}
	yes := true
	focusWords, awayWords := "Focus time is yours alone: it has no guests.", "An out of office is yours alone: it has no guests."
	for _, c := range []struct {
		in          EventInput
		words       string
		focus, away *bool
	}{
		{EventInput{Kind: EventFocus, Title: "Deep work"}, focusWords, &yes, nil},
		{EventInput{Kind: EventAway, Title: "Out of office"}, awayWords, nil, &yes},
		{EventInput{Kind: EventWorkingLocation, Title: "Home", Location: "Home"}, "A working location is yours alone: it has no guests.", nil, nil},
		{EventInput{Kind: EventReminder, Title: "Call the bank"}, "A reminder is yours alone: it has no guests.", nil, nil},
		{EventInput{Kind: EventTaskBlock, TaskUUID: briefTask}, "A task block is yours alone: it has no guests.", nil, nil},
	} {
		kind, in := string(c.in.Kind), c.in
		in.Start, in.End, in.Attendees = start, end, guests
		_, err := h.s.Create(ctx, maya, in)
		refused(kind+" made with a guest", err, c.words)
		in.Attendees = herself
		e, err := h.s.Create(ctx, maya, in)
		if err != nil {
			t.Errorf("%s made naming only its owner: %v", kind, err)
			continue
		}
		_, err = h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(e.Sequence), Attendees: &guests})
		refused(kind+": a guest added", err, c.words)
		err = h.s.legacyUpdateAt(ctx, e.ID, adapter.CreateOrUpdateEventInput{Title: in.Title, StartTime: from, EndTime: to,
			IsFocus: c.focus, IsAway: c.away, Participants: []string{lee.UserDgraphInfo.Uuid}}, maya)
		refused(kind+": a guest added through /event/updateEvent", err, c.words)
	}

	_, err := h.s.legacyCreate(ctx, maya, adapter.CreateOrUpdateEventInput{Title: "Deep work", StartTime: from, EndTime: to, IsFocus: &yes,
		Participants: []string{lee.UserDgraphInfo.Uuid}})
	refused("focus time made with a guest through /event/createEvent", err, focusWords)
	_, err = h.s.legacyCreate(ctx, maya, adapter.CreateOrUpdateEventInput{Title: "Away", StartTime: from, EndTime: to, IsAway: &yes,
		Participants: []string{lee.UserDgraphInfo.Uuid}})
	refused("an out of office made with a guest through /event/createEvent", err, awayWords)

	// A meeting with guests is made own time only without them.
	sync, err := h.s.Create(ctx, maya, EventInput{Title: "Sync", Start: start, End: end, Attendees: guests})
	if err != nil {
		t.Fatal(err)
	}
	focus, none := EventFocus, []GuestInput{}
	_, err = h.s.Update(ctx, maya, sync.ID, EventPatch{Sequence: seq(sync.Sequence), Kind: &focus})
	refused("a meeting made focus time", err, focusWords)
	err = h.s.legacyUpdateAt(ctx, sync.ID, adapter.CreateOrUpdateEventInput{Title: "Sync", StartTime: from, EndTime: to, IsAway: &yes}, maya)
	refused("a meeting made an out of office through /event/updateEvent", err, awayWords)
	if _, err := h.s.Update(ctx, maya, sync.ID, EventPatch{Sequence: seq(sync.Sequence), Kind: &focus, Attendees: &none}); err != nil {
		t.Errorf("a meeting made focus time without its guests: %v", err)
	}

	// An out of office from before the rule that has a guest is still changed
	// in other ways: the form can't take its guests off.
	old, err := h.s.Create(ctx, maya, EventInput{Title: "Offsite", Start: start, End: end, Attendees: guests})
	if err != nil {
		t.Fatal(err)
	}
	h.store.mu.Lock()
	h.store.events[uuid.MustParse(old.ID)].Kind = string(EventAway)
	h.store.mu.Unlock()
	renamed := "Offsite, away"
	if _, err := h.s.Update(ctx, maya, old.ID, EventPatch{Sequence: seq(old.Sequence), Title: &renamed}); err != nil {
		t.Errorf("an older out of office with a guest, renamed: %v", err)
	}
	if err := h.s.legacyUpdateAt(ctx, old.ID, adapter.CreateOrUpdateEventInput{Title: "Offsite", StartTime: from, EndTime: to, IsAway: &yes,
		Participants: []string{lee.UserDgraphInfo.Uuid}}, maya); err != nil {
		t.Errorf("an older out of office with a guest, changed through /event/updateEvent naming that guest: %v", err)
	}
}
