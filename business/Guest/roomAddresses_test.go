package business

import "testing"

// A room's address was made from its owner's email when it is the email's
// words, or those with the eight random letters a taken address got; any
// other address (a name, one its owner chose) was not.
func TestARoomsAddressMadeFromItsOwnersEmailIsKnown(t *testing.T) {
	for _, c := range []struct {
		slug, email string
		made        bool
	}{
		{"visitordemo-onemana-dev", "visitor@demo.onemana.dev", true},
		{"sam-riveraexample-test", "sam.rivera@example.test", true},
		{"sam-riveraexample-test-0f3a9c2e", "sam.rivera@example.test", true},
		{"sam-riveraexample-test-0f3a9c2", "sam.rivera@example.test", false},
		{"sam-riveraexample-test-0f3a9c2z", "sam.rivera@example.test", false},
		{"sam-rivera", "sam.rivera@example.test", false},
		{"sam-rivera-0f3a9c2e", "sam.rivera@example.test", false},
		{"kais-standup", "kai@example.test", false},
		{"room", "", false},
		{"maya", "maya", false},
	} {
		if got := madeFromAddress(c.slug, c.email); got != c.made {
			t.Errorf("%q from %q: %v, want %v", c.slug, c.email, got, c.made)
		}
	}
}
