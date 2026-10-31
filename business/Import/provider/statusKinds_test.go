package provider

import "testing"

// A status with a name of the team's own ("Ready to ship") was proposed
// as To do, and a team that accepted the plan got its work in progress as To
// do. It is now proposed by its class in the source (Linear's state type,
// Jira's status category), and a name the provider knows keeps its own.
func TestAStatusOfTheTeamsOwnIsProposedByItsClass(t *testing.T) {
	defaults := map[string]string{"started": "inProgress", "completed": "done", "canceled": "canceled", "in review": "inReview"}
	kinds := map[string]string{"ready to ship": "started", "won't do": "canceled", "in review": "started", "mystery": "unknown"}
	got := StatusDefaults(defaults, kinds)
	for name, want := range map[string]string{
		"ready to ship": "inProgress", // by its class
		"won't do":      "canceled",
		"in review":     "inReview", // its own name's default wins over its class
		"started":       "inProgress",
	} {
		if v := ApplyStatusMap(name, nil, got); v != want {
			t.Errorf("%q proposed as %q, want %q", name, v, want)
		}
	}
	if v := ApplyStatusMap("mystery", nil, got); v != "todo" {
		t.Errorf("a class with no default falls back as before: %q", v)
	}
	if defaults["ready to ship"] != "" {
		t.Error("the provider's own map must not change")
	}
}
