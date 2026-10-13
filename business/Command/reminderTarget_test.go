package business

import "testing"

// A reminder's target: a person picked from the list comes as their shown
// name, which can hold spaces, with the composer's map of names to ids; a
// typed "@word" is a handle.
func TestWhoAReminderIsFor(t *testing.T) {
	picked := map[string]string{"maya chen": "u-maya", "maya": "u-other", "sam": "u-sam"}
	for _, tc := range []struct {
		text     string
		mentions map[string]string
		want     remindTarget
		rest     string
	}{
		{"@Maya Chen to review the deck tomorrow at 9am", picked, remindTarget{kind: "user", id: "u-maya", name: "Maya Chen"}, "to review the deck tomorrow at 9am"},
		{"@maya to call the bank in 5 minutes", picked, remindTarget{kind: "user", id: "u-other", name: "maya"}, "to call the bank in 5 minutes"},
		{"@Maya Chen", picked, remindTarget{kind: "user", id: "u-maya", name: "Maya Chen"}, ""},
		// Not a picked name: "@sammy" doesn't start with "sam " or end there.
		{"@sammy to stretch in 5 minutes", picked, remindTarget{kind: "user", name: "sammy"}, "to stretch in 5 minutes"},
		// Typed, with no list: a handle.
		{"@maya-chen to review the deck tomorrow", nil, remindTarget{kind: "user", name: "maya-chen"}, "to review the deck tomorrow"},
		{"me to stretch in 5 minutes", picked, remindTarget{kind: "self"}, "to stretch in 5 minutes"},
		{"#launch standup at 10am", picked, remindTarget{kind: "channel", name: "launch"}, "standup at 10am"},
		{"to stretch in 5 minutes", picked, remindTarget{kind: "self"}, "to stretch in 5 minutes"},
	} {
		got, rest := parseRemindTarget(tc.text, tc.mentions)
		if got != tc.want || rest != tc.rest {
			t.Errorf("%q: %+v %q, want %+v %q", tc.text, got, rest, tc.want, tc.rest)
		}
	}
}
