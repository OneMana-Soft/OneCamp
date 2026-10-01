package business

import (
	"strings"
	"testing"

	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestClipPrepText(t *testing.T) {
	if got := clipPrepText("hello", 10); got != "hello" {
		t.Fatalf("short text should pass through, got %q", got)
	}
	got := clipPrepText("hello world", 5)
	if got != "hello…" {
		t.Fatalf("expected clipped with ellipsis, got %q", got)
	}
	// Rune-safe: multibyte content must not be split mid-rune.
	multi := clipPrepText("日本語テキスト", 3)
	if multi != "日本語…" {
		t.Fatalf("expected rune-safe clip, got %q", multi)
	}
}

func TestBuildPrepContext_SectionsAndGrounding(t *testing.T) {
	related := []ai.SimilarResult{
		{ContentText: "<p>We agreed to ship the billing page</p>", ChannelName: "product", AuthorName: "alice"},
		{ContentText: "", ChannelName: "noise"}, // empty → skipped
	}
	items := []*memoryModels.MemoryItem{
		{Kind: memoryModels.KindCommitment, Content: "Send the pricing doc"},
		nil, // nil → skipped
	}

	out := buildPrepContext(
		"Q3 Planning",
		"2026-07-01T09:00:00Z",
		[]string{"alice", "bob"},
		"Plan the quarter",
		related,
		items,
	)

	for _, want := range []string{
		"MEETING",
		"Title: Q3 Planning",
		"When: 2026-07-01T09:00:00Z",
		"Attendees: alice, bob",
		"Description: Plan the quarter",
		"RELATED DISCUSSION",
		"[product, alice]",
		"ship the billing page", // HTML stripped
		"ORGANIZER OPEN ITEMS",
		"(commitment) Send the pricing doc",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("context missing %q\n---\n%s", want, out)
		}
	}

	// HTML tags must be stripped from related snippets.
	if strings.Contains(out, "<p>") {
		t.Fatalf("related snippet HTML not stripped:\n%s", out)
	}
}

func TestBuildPrepContext_OmitsEmptySections(t *testing.T) {
	out := buildPrepContext("Standup", "", nil, "", nil, nil)
	if strings.Contains(out, "RELATED DISCUSSION") {
		t.Fatalf("should omit related section when none provided:\n%s", out)
	}
	if strings.Contains(out, "ORGANIZER OPEN ITEMS") {
		t.Fatalf("should omit open items section when none provided:\n%s", out)
	}
	if !strings.Contains(out, "Title: Standup") {
		t.Fatalf("title should still be present:\n%s", out)
	}
}
