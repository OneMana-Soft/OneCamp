package provider

import "testing"

// I1: a source's own keys (Jira's and Linear's "ENG-123") come across, so a
// moved team's links, commits and habits still find the task.
func TestASourceKeySaysItsPrefixAndNumber(t *testing.T) {
	for raw, want := range map[string]struct {
		prefix string
		n      int
	}{
		"ENG-123": {"ENG", 123}, "eng-7": {"ENG", 7}, "Q4-1": {"Q4", 1},
		"ENG": {"", 0}, "ENG-": {"", 0}, "ENG-12a": {"", 0}, "-12": {"", 0}, "": {"", 0}, "ENG-0": {"", 0},
	} {
		p, n := SplitKey(raw)
		if p != want.prefix || n != want.n {
			t.Errorf("SplitKey(%q) = %q, %d; want %q, %d", raw, p, n, want.prefix, want.n)
		}
	}
}
