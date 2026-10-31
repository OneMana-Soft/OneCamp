package business

import (
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// Calls package C, breakout rooms: a room is its parent call's, named from the
// parent's own room and numbered, so the webhooks and every room-keyed check
// find whose call it is. Channel calls and meetings have rooms; a DM's or a
// group's call has none.
func TestABreakoutRoomIsItsCallsAndNumbered(t *testing.T) {
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	meeting := "meet-6f1c0e2a-1111-4222-8333-944455556666"
	for _, c := range []struct {
		parent, kind, id string
		n                int
	}{
		{channel, "channel", channel, 1},
		{channel, "channel", channel, MaxBreakouts},
		{meeting, "meeting", "6f1c0e2a-1111-4222-8333-944455556666", 7},
	} {
		room := BreakoutRoom(c.parent, c.n)
		if room == c.parent {
			t.Fatalf("room %d of %q is named as its parent", c.n, c.parent)
		}
		parent, n, ok := ParentOfBreakout(room)
		if !ok || parent != c.parent || n != c.n {
			t.Errorf("%q is room %d of %q (%v), want room %d of %q", room, n, parent, ok, c.n, c.parent)
		}
		ref, kind, ok := RefOfRoom(room)
		if !ok || ref.Kind != c.kind || ref.ID != c.id || kind != model.KindBreakout {
			t.Errorf("%q is %+v %q %v, want a breakout of the %s %s", room, ref, kind, ok, c.kind, c.id)
		}
	}
	dm := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33 5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44"
	group := "0123456789abcdef0123456789abcdef"
	for _, room := range []string{
		channel, meeting, "",
		channel + "~b0", channel + "~b01", channel + "~bx", channel + "~b", channel + "~b51",
		BreakoutRoom(channel, 2) + "~b1", // a room of a room
		dm + "~b1", group + "~b1",        // DMs and groups have no rooms
		"meet-~b1", "general~b1",
	} {
		if _, _, ok := ParentOfBreakout(room); ok {
			t.Errorf("%q was taken for a breakout room", room)
		}
		if _, kind, ok := RefOfRoom(room); ok && kind == model.KindBreakout {
			t.Errorf("%q was taken for a breakout room's call", room)
		}
	}
}

// While a call's people are in its breakout rooms its own room may empty, and
// LiveKit may close it and make it again: the call goes on in its rooms while
// they are open or counting down, and for a little while after everyone was
// called back, so their way back is no new call.
func TestACallGoesOnInItsRoomsUntilEveryoneIsBack(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time {
		t := now.Add(d)
		return &t
	}
	for name, c := range map[string]struct {
		s    model.Session
		want bool
	}{
		"rooms never opened":        {model.Session{}, false},
		"rooms open":                {model.Session{BreakoutsOpenAt: at(-time.Minute)}, true},
		"rooms counting down":       {model.Session{BreakoutsOpenAt: at(-time.Minute), BreakoutsClosingAt: at(20 * time.Second)}, true},
		"everyone just called back": {model.Session{BreakoutsClosingAt: at(-30 * time.Second)}, true},
		"everyone back a while ago": {model.Session{BreakoutsClosingAt: at(-model.BreakoutReturnGrace - time.Second)}, false},
	} {
		if got := c.s.GoesOnInRooms(now); got != c.want {
			t.Errorf("%s: goes on in its rooms %v, want %v", name, got, c.want)
		}
	}
}
