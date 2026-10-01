package codepr

import (
	"strings"
	"testing"
)

func TestParseSelectors_BareArray(t *testing.T) {
	raw := `[{"kind":"lexical","query":"parseConfig"},{"kind":"import_graph","query":"config/loader.go","language":"go"}]`
	got := ParseSelectors(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 selectors, got %d (%+v)", len(got), got)
	}
	if got[0].Kind != SelectorLexical || got[0].Query != "parseConfig" {
		t.Fatalf("selector 0 wrong: %+v", got[0])
	}
	if got[1].Kind != SelectorImportGraph || got[1].Language != "go" {
		t.Fatalf("selector 1 wrong: %+v", got[1])
	}
}

func TestParseSelectors_FencedAndProse(t *testing.T) {
	raw := "Here are the selectors:\n```json\n[{\"type\":\"tree-sitter\",\"symbol\":\"HandleLogin\"}]\n```\nUse them."
	got := ParseSelectors(raw)
	if len(got) != 1 || got[0].Kind != SelectorTreeSitter || got[0].Query != "HandleLogin" {
		t.Fatalf("fenced+prose+synonyms not parsed: %+v", got)
	}
}

func TestParseSelectors_SingleObjectWrapped(t *testing.T) {
	got := ParseSelectors(`{"kind":"rg","query":"TODO"}`)
	if len(got) != 1 || got[0].Kind != SelectorLexical {
		t.Fatalf("single object should wrap to one lexical selector: %+v", got)
	}
}

func TestParseSelectors_UnparseableIsNil(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "```\nnope\n```"} {
		if got := ParseSelectors(raw); got != nil {
			t.Fatalf("unparseable %q must yield nil, got %+v", raw, got)
		}
	}
}

func TestParseSelectors_DropsEmptyAndUnknownKindDefaults(t *testing.T) {
	raw := `[{"kind":"lexical","query":""},{"kind":"weird","query":"X"}]`
	got := ParseSelectors(raw)
	// First dropped (empty query); second kept with lexical default.
	if len(got) != 1 || got[0].Kind != SelectorLexical || got[0].Query != "X" {
		t.Fatalf("expected one defaulted lexical selector, got %+v", got)
	}
}

func TestSanitizeSelectors_DedupeAndCap(t *testing.T) {
	in := []Selector{
		{Kind: SelectorLexical, Query: "foo"},
		{Kind: SelectorLexical, Query: "foo"},                                        // dup
		{Kind: SelectorLexical, Query: "  "},                                         // empty after trim
		{Kind: SelectorLexical, Query: strings.Repeat("x", maxSelectorQueryChars+1)}, // oversized
	}
	got := SanitizeSelectors(in)
	if len(got) != 1 || got[0].Query != "foo" {
		t.Fatalf("expected one deduped selector, got %+v", got)
	}

	// Cap enforcement.
	big := make([]Selector, maxSelectors+5)
	for i := range big {
		big[i] = Selector{Kind: SelectorLexical, Query: string(rune('a' + i))}
	}
	if got := SanitizeSelectors(big); len(got) != maxSelectors {
		t.Fatalf("expected cap at %d, got %d", maxSelectors, len(got))
	}
}

func TestBuildSelectorPrompt_Deterministic(t *testing.T) {
	msgs := buildSelectorPrompt("rename Foo to Bar everywhere")
	if len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(msgs[1].Content, "rename Foo") {
		t.Fatalf("unexpected selector prompt: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "UNTRUSTED DATA") {
		t.Fatal("selector prompt must harden against injection")
	}
}
