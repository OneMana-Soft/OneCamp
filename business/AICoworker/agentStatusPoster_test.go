package aicoworker

import "testing"

func TestProgressToolsLine(t *testing.T) {
	if got := progressToolsLine(nil); got != "" {
		t.Errorf("nil tools should yield empty, got %q", got)
	}
	if got := progressToolsLine([]string{"", "  "}); got != "" {
		t.Errorf("blank tools should yield empty, got %q", got)
	}
	// Humanizes (underscores → spaces), dedups, preserves order.
	got := progressToolsLine([]string{"list_commits", "list_commits", "repo_summary"})
	if got != "(using: list commits, repo summary)" {
		t.Errorf("unexpected line: %q", got)
	}
	// Caps at 4 distinct tools.
	got = progressToolsLine([]string{"a", "b", "c", "d", "e", "f"})
	if got != "(using: a, b, c, d)" {
		t.Errorf("expected cap at 4, got %q", got)
	}
}
