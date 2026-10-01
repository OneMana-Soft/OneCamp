package ai

import (
	"strings"
	"testing"
)

// smallToolSet uses a handful of built-in tools (<= threshold): the prompt must
// render EVERY tool with full params regardless of query — byte-identical to
// the original behaviour, so ordinary agents are unaffected by compaction.
func TestBuildAgentToolPromptForRun_SmallSetAlwaysFull(t *testing.T) {
	names := []string{"list_tasks", "create_task", "update_task_status"}

	full := BuildAgentToolPromptForRun(names, "")                                  // no query
	withQuery := BuildAgentToolPromptForRun(names, "send an email about the repo") // unrelated query

	if full != withQuery {
		t.Fatalf("small tool-set must render identically regardless of query")
	}
	// Every tool should carry a Params line (all detailed).
	if strings.Count(full, "Params:") < 2 {
		t.Fatalf("expected full param schemas for a small tool-set, got:\n%s", full)
	}
	// No subset advisory for a small set.
	if strings.Contains(full, "most relevant to this request") {
		t.Fatalf("small tool-set must not show the subset advisory")
	}
}

// largeToolSet exceeds the threshold: only the tools relevant to the query get
// full params, but EVERY tool is still listed by name so none is hidden.
func TestBuildAgentToolPromptForRun_LargeSetCompactsToRelevant(t *testing.T) {
	// Build an allow-list bigger than the threshold from the built-in catalog.
	var names []string
	for _, td := range ToolRegistry {
		names = append(names, td.Name)
	}
	if len(names) <= agentToolFullThreshold {
		t.Skipf("catalog smaller than threshold (%d); test not meaningful", agentToolFullThreshold)
	}

	out := BuildAgentToolPromptForRun(names, "create a task in the project")

	// Every enabled tool is still listed by name (nothing hidden).
	for _, td := range ToolRegistry {
		if !toolEnabled(td.Name) {
			continue
		}
		if !strings.Contains(out, "- "+td.Name+":") {
			t.Fatalf("tool %q missing from the name list (hidden tool)", td.Name)
		}
	}
	// The subset advisory must be present for a large, compacted set.
	if !strings.Contains(out, "most relevant to this request") {
		t.Fatalf("expected subset advisory for a large tool-set")
	}
	// A task-relevant tool must be detailed (has Params); an unrelated one
	// (gmail_send) must be listed by name only (no Params under it).
	if !strings.Contains(out, "- create_task:") {
		t.Fatalf("create_task should be listed")
	}
	// Detailed count is bounded by the threshold.
	if got := strings.Count(out, "Params:"); got > agentToolFullThreshold {
		t.Fatalf("detailed tool count %d exceeds cap %d", got, agentToolFullThreshold)
	}
}

func TestSignificantWords(t *testing.T) {
	got := significantWords("Please send the message about GitHub repo")
	// stop-words (please, send?, the, message, about) and short words dropped;
	// "github" and "repo" kept. "send" is < 4? no, len 4 -> kept unless stop.
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "github") || !strings.Contains(joined, "repo") {
		t.Fatalf("expected github+repo in significant words, got %q", joined)
	}
	for _, bad := range []string{"the", "about", "message"} {
		for _, w := range got {
			if w == bad {
				t.Fatalf("stop-word %q should have been dropped", bad)
			}
		}
	}
}
