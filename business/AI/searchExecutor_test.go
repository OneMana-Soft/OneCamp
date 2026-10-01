package business

import (
	"strings"
	"testing"
)

func TestRenderUnifiedSearchForModel(t *testing.T) {
	resp := &UnifiedSearchResponse{
		Enabled: true,
		Query:   "billing",
		Groups: []UnifiedSearchGroup{
			{Source: "workspace", Label: "Workspace", Connected: true, Hits: []UnifiedHit{
				{Title: "Post by alice", Meta: "#product", Snippet: "ship the billing page"},
			}},
			{Source: "memory", Label: "Memory", Connected: true, Hits: []UnifiedHit{
				{Title: "Ship billing Friday", Kind: "commitment", Meta: "#product · due 2026-07-03"},
			}},
			{Source: "github", Label: "GitHub", Connected: true, Hits: []UnifiedHit{
				{Title: "Fix billing bug", Meta: "acme/api #12 · open", URL: "https://gh/1"},
			}},
			{Source: "gmail", Label: "Gmail", Connected: false, Hits: nil}, // empty → omitted
		},
	}
	out := renderUnifiedSearchForModel(resp)

	for _, want := range []string{
		"## Workspace",
		"Post by alice (#product)",
		"ship the billing page",
		"## Memory",
		"Ship billing Friday",
		"## GitHub",
		"acme/api #12 · open",
		"https://gh/1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("digest missing %q\n---\n%s", want, out)
		}
	}
	// Empty group must be omitted.
	if strings.Contains(out, "## Gmail") {
		t.Fatalf("empty group should be omitted:\n%s", out)
	}
}

func TestRenderUnifiedSearchForModel_Empty(t *testing.T) {
	resp := &UnifiedSearchResponse{
		Enabled: true,
		Query:   "nothing",
		Groups: []UnifiedSearchGroup{
			{Source: "workspace", Label: "Workspace", Connected: true, Hits: nil},
		},
	}
	out := renderUnifiedSearchForModel(resp)
	if !strings.Contains(out, "No results found") || !strings.Contains(out, "nothing") {
		t.Fatalf("expected a no-results message, got %q", out)
	}
}
