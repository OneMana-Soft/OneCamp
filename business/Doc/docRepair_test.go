package businness

import "testing"

// A save from the collaboration service may never replace a doc with nothing
// or with the placeholder services before v2.70.0 wrote when they could not
// write its HTML.
func TestACollabSaveRefusesBlankAndPlaceholderBodies(t *testing.T) {
	for _, body := range []string{"", "   ", "<p>Error generating HTML</p>", "\n<p>Error generating HTML</p> "} {
		if !unsafeCollabBody(body) {
			t.Errorf("%q was accepted", body)
		}
	}
	for _, body := range []string{"<p>Ship on Thursday.</p>", "<p>Error generating HTML</p><p>and more</p>"} {
		if unsafeCollabBody(body) {
			t.Errorf("%q was refused", body)
		}
	}
}

// repairdocs rebuilds a body from its saved state only when the body is very
// short and the state clearly holds more: a doc someone simply cut down stays
// as they left it.
func TestRepairRebuildsOnlyAShortBodyBesideAFullerState(t *testing.T) {
	full := "<h1>Launch plan</h1><p>Ship on Thursday, after the review with the design team and legal.</p>"
	cases := []struct {
		name, body, rendered string
		want                 bool
	}{
		{"short body, much fuller state", "<p>Plan</p>", full, true},
		{"the same body", full, full, false},
		{"a long body is never touched", full, full + "<p>more</p>", false},
		{"short body, state no fuller", "<p>Plan</p>", "<p>Plan B</p>", false},
		{"a state that writes the placeholder", "<p>Plan</p>", "<p>Error generating HTML</p>", false},
		{"a state that writes nothing", "<p>Plan</p>", "", false},
	}
	for _, tc := range cases {
		if got := repairWantsState(tc.body, tc.rendered); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
