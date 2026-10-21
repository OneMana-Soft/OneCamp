package taskpriority

import "testing"

// One priority scale, as Linear, Jira and Asana have it: urgent, high,
// medium, low and none. A task's priority could be only low, medium or high,
// so "No priority" was refused and every importer's urgent became high.
func TestOnePriorityScale(t *testing.T) {
	for _, p := range []string{"urgent", "high", "medium", "low"} {
		if !Is(p) {
			t.Fatalf("%q is a priority", p)
		}
	}
	if Is("") || Is("p1") || !IsOrNone("") || IsOrNone("p1") {
		t.Fatal("none is a choice, not a priority; p1 is neither")
	}
	order := []string{"urgent", "high", "medium", "low", ""}
	for i := 1; i < len(order); i++ {
		if Rank(order[i-1]) >= Rank(order[i]) {
			t.Fatalf("%q sorts before %q", order[i-1], order[i])
		}
	}
	for words, want := range map[string]string{
		"Urgent": "urgent", "critical": "urgent", "Highest": "urgent", "p0": "urgent", "blocker": "urgent",
		"high": "high", "very high": "high", "p1": "high", "major": "high",
		"Medium": "medium", "normal": "medium", "p2": "medium", "p3": "medium",
		"low": "low", "lowest": "low", "minor": "low", "trivial": "low", "p4": "low", "p5": "low", "best effort": "low",
		"No priority": "", "none": "", "": "",
		"weird": "?",
	} {
		got, ok := FromWords(words)
		if want == "?" {
			if ok {
				t.Fatalf("%q read as %q", words, got)
			}
			continue
		}
		if !ok || got != want {
			t.Fatalf("%q read as %q (%v), want %q", words, got, ok, want)
		}
	}
}
