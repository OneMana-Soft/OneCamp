package helpers

import "testing"

// A DM opens at the other person, whichever side reads the link; a group at
// its grouping id; a message at its thread. The web app's chatPath answers
// the same cases the same way.
func TestChatPathOpensTheConversationForWhoeverReadsIt(t *testing.T) {
	const maya, jonas, group = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "0123456789abcdef0123456789abcdef"
	dm := GetGroupingId(maya, jonas)
	cases := []struct {
		name, grp, viewer, msg, want string
	}{
		{"a DM, as Maya, opens at Jonas", dm, maya, "", "/app/chat/" + jonas},
		{"the same DM, as Jonas, opens at Maya", dm, jonas, "", "/app/chat/" + maya},
		{"a DM thread opens at its message", dm, maya, "m1", "/app/chat/" + jonas + "/m1"},
		{"a group opens at its grouping id", group, maya, "", "/app/chat/group/" + group},
		{"a group thread opens at its message", group, jonas, "m1", "/app/chat/group/" + group + "/m1"},
		{"a DM with oneself opens at oneself", GetGroupingId(maya, maya), maya, "", "/app/chat/" + maya},
		{"no grouping id, no link", "", maya, "m1", ""},
	}
	for _, c := range cases {
		if got := ChatPath(c.grp, c.viewer, c.msg); got != c.want {
			t.Errorf("%s: ChatPath = %q, want %q", c.name, got, c.want)
		}
	}
}
