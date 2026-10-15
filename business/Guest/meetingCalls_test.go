package business

import "testing"

// A guest's call identity names the link they joined by, which is how a
// revoked link finds its guests in the meeting (CALL-BUG-11).
func TestGrantOfGuest(t *testing.T) {
	grant := "6f1c0e2a-1111-4222-8333-944455556666"
	for identity, want := range map[string]string{
		guestIdentityPrefix(grant) + "abc-_def":     grant,
		guestIdentityPrefix(grant) + "x":            grant,
		"guest-" + grant:                            "",
		"guest-not-a-uuid-at-all-but-long-enough-x": "",
		"0x1a":            "",
		"transcriber-bot": "",
	} {
		if got := grantOfGuest(identity); got != want {
			t.Errorf("grantOfGuest(%q) = %q, want %q", identity, got, want)
		}
	}
}

// An instant meeting's id is a uuid, and its room is "meet-" and that id.
func TestMeetingRoom(t *testing.T) {
	if room, ok := MeetingRoom("6F1C0E2A-1111-4222-8333-944455556666"); !ok || room != "meet-6f1c0e2a-1111-4222-8333-944455556666" {
		t.Errorf("room = %q %v", room, ok)
	}
	for _, bad := range []string{"", "meet-x", "../admin", "6f1c0e2a"} {
		if _, ok := MeetingRoom(bad); ok {
			t.Errorf("%q made a meeting room", bad)
		}
	}
}
