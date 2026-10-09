package controllers

import "testing"

// A name is checked by the person rule only when it changes: one already
// saved, whatever made it, never stops someone saving the rest of their
// profile, and the names sign-up and identity providers produce are names.
func TestAProfileNameIsCheckedOnlyWhenItChanges(t *testing.T) {
	cases := []struct {
		sent, saved string
		refused     bool
	}{
		{"José O'Brien", "Jose", false},
		{"priya.raman", "", false},
		{"Ada Augusta King Countess of Lovelace", "x", false}, // over 25 bytes
		{"sam_1a2b3", "sam_1a2b3", false},                     // saved before the rule: kept
		{" sam_1a2b3 ", "sam_1a2b3", false},
		{"sam_1a2b3", "sam", true}, // changed to something the rule refuses
		{"<b>bold</b>", "Sam", true},
		{"", "Sam", false}, // empty keeps what is saved
	}
	for _, c := range cases {
		if got := changedNameProblem("Display name", c.sent, c.saved) != ""; got != c.refused {
			t.Errorf("sent %q over %q: refused %v, want %v", c.sent, c.saved, got, c.refused)
		}
	}
}
