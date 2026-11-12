package models

import "testing"

// A new agent, or one saved with no readable autonomy, asks first: each
// change waits for a yes in the Inbox (ADMIN-16). A chosen mode is kept.
func TestCreateAgentDefaultsToApproval(t *testing.T) {
	for _, a := range []string{"", "  ", "bogus", "AUTO"} {
		if got := AutonomyOrDefault(a); got != AutonomyApproval {
			t.Errorf("autonomy %q became %q, want %q", a, got, AutonomyApproval)
		}
	}
	for _, a := range []string{AutonomyAuto, AutonomyApproval, AutonomyPlan} {
		if got := AutonomyOrDefault(a); got != a {
			t.Errorf("a chosen %q became %q", a, got)
		}
	}
}
