package helpers

import "testing"

// A room name is overloaded and the three shapes look similar enough that a
// caller writing the check inline gets it subtly wrong. These pin each shape.

func TestAChannelCallResolvesToItsChannel(t *testing.T) {
	// A channel room name IS the channel uuid, hyphens and all.
	ch, grp := ClassifyRoom("6f1c4c2e-9a1b-4d3e-9f77-1b2c3d4e5f60")
	if ch != "6f1c4c2e-9a1b-4d3e-9f77-1b2c3d4e5f60" {
		t.Errorf("channel uuid lost: got %q", ch)
	}
	if grp != "" {
		t.Errorf("a channel call was also reported as a chat: %q", grp)
	}
}

func TestAGroupChatResolvesToItsGrouping(t *testing.T) {
	// Space-joined sorted uuids. The space is the giveaway.
	const grouping = "aaaa1111-2222-3333-4444-555566667777 bbbb1111-2222-3333-4444-555566667777"
	ch, grp := ClassifyRoom(grouping)
	if grp != grouping {
		t.Errorf("grouping id lost: got %q", grp)
	}
	if ch != "" {
		t.Errorf("a chat was reported as a channel: %q", ch)
	}
}

func TestATwoPersonChatResolvesToItsGrouping(t *testing.T) {
	// 32 characters, no hyphens: a uuid with the dashes stripped.
	const grouping = "6f1c4c2e9a1b4d3e9f771b2c3d4e5f60"
	ch, grp := ClassifyRoom(grouping)
	if grp != grouping || ch != "" {
		t.Errorf("expected a chat grouping, got channel=%q chat=%q", ch, grp)
	}
}

func TestAnInstantMeetingHasNoSurface(t *testing.T) {
	// THE CASE THAT MATTERS. An instant meeting belongs to no conversation. If
	// it fell through to the default it would be reported as a channel uuid
	// that does not exist, and every caller would go looking for it.
	ch, grp := ClassifyRoom("meet-6f1c4c2e-9a1b-4d3e-9f77-1b2c3d4e5f60")
	if ch != "" || grp != "" {
		t.Errorf("an instant meeting resolved to a surface: channel=%q chat=%q", ch, grp)
	}
}

func TestNothingInNothingOut(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if ch, grp := ClassifyRoom(in); ch != "" || grp != "" {
			t.Errorf("input %q produced channel=%q chat=%q", in, ch, grp)
		}
	}
}

func TestExactlyOneSurfaceIsEverReturned(t *testing.T) {
	// The property every caller relies on: it is one or the other, never both.
	for _, in := range []string{
		"6f1c4c2e-9a1b-4d3e-9f77-1b2c3d4e5f60",
		"aaaa1111-2222-3333-4444-555566667777 bbbb1111-2222-3333-4444-555566667777",
		"6f1c4c2e9a1b4d3e9f771b2c3d4e5f60",
		"meet-abc",
		"",
	} {
		if ch, grp := ClassifyRoom(in); ch != "" && grp != "" {
			t.Errorf("input %q claimed to be both a channel and a chat", in)
		}
	}
}
