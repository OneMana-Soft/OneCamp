package business

import "testing"

// I1, the framework's mapping for every provider: a task keeps all its labels
// (only the first was kept), and one the source says is completed is closed
// whatever its column was called (Asana's "To do" section reopened closed
// work).

func TestATaskKeepsEveryLabel(t *testing.T) {
	if got := taskLabel([]string{"Backend", "urgent", "backend", " ", "Q4"}); got != "Backend, urgent, Q4" {
		t.Fatalf("labels %q", got)
	}
	if got := taskLabel(nil); got != "" {
		t.Fatalf("no labels %q", got)
	}
}

func TestACompletedTaskIsClosedWhateverItsColumn(t *testing.T) {
	for _, c := range []struct {
		mapped    string
		completed bool
		want      string
	}{
		{"todo", true, "done"},
		{"inProgress", true, "done"},
		{"canceled", true, "canceled"},
		{"done", true, "done"},
		{"todo", false, "todo"},
	} {
		if got := closedIfCompleted(c.mapped, c.completed); got != c.want {
			t.Errorf("%s completed=%v: %s, want %s", c.mapped, c.completed, got, c.want)
		}
	}
}

// I1: a source's estimate is kept within what a task can hold (a thousand
// hours); none or a negative one is none.
func TestAnEstimateIsKeptWithinWhatATaskHolds(t *testing.T) {
	for in, want := range map[int]int{0: 0, -5: 0, 90: 90, 2_000_000: 60_000} {
		if got := estimateMinutes(in); got != want {
			t.Fatalf("estimateMinutes(%d) = %d, want %d", in, got, want)
		}
	}
}
