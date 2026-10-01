package ai

import "testing"

// The rule that decides who may read or extend a conversation.
//
// It is tested directly because the bug it replaces was invisible at the call
// site: the old form skipped the comparison when no owner was recorded, and a
// skipped comparison is indistinguishable from a passed one at a glance.
func TestSessionBelongsTo(t *testing.T) {
	const owner = "11111111-1111-1111-1111-111111111111"
	const other = "22222222-2222-2222-2222-222222222222"

	cases := []struct {
		name   string
		stored string
		caller string
		want   bool
	}{
		{"the owner comes back", owner, owner, true},
		{"someone else holding the id", owner, other, false},
		{"an unowned record is not everyone's", "", owner, false},
		{"an anonymous caller owns nothing", owner, "", false},
		{"blank on both sides is not a match", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionBelongsTo(c.stored, c.caller); got != c.want {
				t.Fatalf("sessionBelongsTo(%q, %q) = %v, want %v", c.stored, c.caller, got, c.want)
			}
		})
	}
}
