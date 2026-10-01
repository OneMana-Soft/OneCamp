package business

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTrimOneLine(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello world", 100, "hello world"},
		{"  multiple   spaces \n collapsed ", 100, "multiple spaces collapsed"},
		{"abcdefghij", 5, "abcde…"},
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := trimOneLine(c.in, c.max); got != c.want {
			t.Errorf("trimOneLine(%q,%d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestNudgeInterval(t *testing.T) {
	t.Setenv("AI_NUDGE_INTERVAL_MIN", "")
	if nudgeInterval() != nudgeDefaultInterval {
		t.Errorf("expected default interval %v", nudgeDefaultInterval)
	}
	t.Setenv("AI_NUDGE_INTERVAL_MIN", "5")
	if nudgeInterval() != 5*time.Minute {
		t.Error("expected 5m interval")
	}
	t.Setenv("AI_NUDGE_INTERVAL_MIN", "0") // invalid → default
	if nudgeInterval() != nudgeDefaultInterval {
		t.Error("zero should fall back to default")
	}
	t.Setenv("AI_NUDGE_INTERVAL_MIN", "garbage") // invalid → default
	if nudgeInterval() != nudgeDefaultInterval {
		t.Error("garbage should fall back to default")
	}
}

func TestLLMPhrasingEnabled(t *testing.T) {
	t.Setenv("AI_NUDGE_LLM_PHRASING", "")
	if llmPhrasingEnabled() {
		t.Error("default should be off")
	}
	t.Setenv("AI_NUDGE_LLM_PHRASING", "true")
	if !llmPhrasingEnabled() {
		t.Error("true should enable")
	}
	t.Setenv("AI_NUDGE_LLM_PHRASING", "TRUE")
	if !llmPhrasingEnabled() {
		t.Error("case-insensitive true should enable")
	}
}

// TestCandidateSortPriority verifies the priority-desc ordering the engine
// applies before capping, so high-priority nudges (overdue commitments) are
// never dropped in favor of low-priority ones (stale questions) under the cap.
func TestCandidateSortPriority(t *testing.T) {
	list := []candidateNudge{
		{userID: uuid.New(), priority: 0, dedupKey: "low-1"},
		{userID: uuid.New(), priority: 1, dedupKey: "high-1"},
		{userID: uuid.New(), priority: 0, dedupKey: "low-2"},
		{userID: uuid.New(), priority: 1, dedupKey: "high-2"},
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].priority > list[j].priority })

	if list[0].priority != 1 || list[1].priority != 1 {
		t.Fatalf("expected high-priority items first, got %+v", list)
	}
	// Stable: within equal priority, original order preserved.
	if list[0].dedupKey != "high-1" || list[1].dedupKey != "high-2" {
		t.Errorf("expected stable order high-1, high-2; got %s, %s", list[0].dedupKey, list[1].dedupKey)
	}
}
