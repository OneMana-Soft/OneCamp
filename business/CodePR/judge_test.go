package codepr

import (
	"strings"
	"testing"
)

func TestParseScopeVerdict_BareObject(t *testing.T) {
	v := parseScopeVerdict(`{"in_scope": true, "concern": ""}`)
	if !v.InScope || v.Concern != "" {
		t.Fatalf("expected in-scope, no concern; got %+v", v)
	}

	v = parseScopeVerdict(`{"in_scope": false, "concern": "also refactors unrelated files"}`)
	if v.InScope || v.Concern == "" {
		t.Fatalf("expected out-of-scope with concern; got %+v", v)
	}
}

func TestParseScopeVerdict_FencedAndProse(t *testing.T) {
	fenced := "Here is my verdict:\n```json\n{\"in_scope\": false, \"concern\": \"disables a test\"}\n```\nthanks"
	v := parseScopeVerdict(fenced)
	if v.InScope || !strings.Contains(v.Concern, "disables") {
		t.Fatalf("fenced+prose verdict not parsed: %+v", v)
	}

	prose := `I think the change {"in_scope": true} looks fine.`
	if got := parseScopeVerdict(prose); !got.InScope {
		t.Fatalf("prose-embedded object not parsed: %+v", got)
	}
}

func TestParseScopeVerdict_KeyAndValueVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool // expected InScope
	}{
		{"string true", `{"in_scope":"yes"}`, true},
		{"string false", `{"in_scope":"no","concern":"x"}`, false},
		{"within_scope", `{"within_scope": true}`, true},
		{"out_of_scope true", `{"out_of_scope": true, "reason":"unrelated bump"}`, false},
		{"out_of_scope false", `{"out_of_scope": false}`, true},
		{"verdict string out", `{"verdict":"out_of_scope","issue":"scope creep"}`, false},
		{"verdict string in", `{"verdict":"in_scope"}`, true},
		{"numeric 1", `{"in_scope": 1}`, true},
		{"numeric 0", `{"in_scope": 0, "concern":"y"}`, false},
		{"concern only", `{"concern":"deletes tests"}`, false},
		{"empty concern only", `{"concern":""}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseScopeVerdict(c.raw); got.InScope != c.want {
				t.Fatalf("InScope=%v, want %v (verdict %+v)", got.InScope, c.want, got)
			}
		})
	}
}

func TestParseScopeVerdict_FailsOpen(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json at all", "```\nnope\n```", "{unbalanced"} {
		if v := parseScopeVerdict(raw); !v.InScope {
			t.Fatalf("unparseable verdict must fail open (in-scope); raw=%q got %+v", raw, v)
		}
	}
}

func TestParseScopeVerdict_InScopeDropsStrayConcern(t *testing.T) {
	// A model that returns in_scope true but leaves a concern string must not
	// surface the concern (it only applies to out-of-scope).
	v := parseScopeVerdict(`{"in_scope": true, "concern": "looks fine but consider X"}`)
	if !v.InScope || v.Concern != "" {
		t.Fatalf("in-scope verdict must carry no concern; got %+v", v)
	}
}

func TestExtractFirstJSONObject_StringAwareBraces(t *testing.T) {
	// A brace inside a string value must not close the object early.
	in := `prefix {"concern":"has a } brace","in_scope":false} suffix`
	obj := extractFirstJSONObject(in)
	if obj != `{"concern":"has a } brace","in_scope":false}` {
		t.Fatalf("string-aware extraction failed: %q", obj)
	}
}

func TestBuildScopeGuardPrompt(t *testing.T) {
	msgs := buildScopeGuardPrompt("fix the padding", "diff --git a/x b/x")
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("unexpected message shape: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "UNTRUSTED DATA") {
		t.Fatal("system prompt must harden against prompt injection")
	}
	if !strings.Contains(msgs[1].Content, "fix the padding") || !strings.Contains(msgs[1].Content, "diff --git") {
		t.Fatalf("user turn must include task and diff: %q", msgs[1].Content)
	}
}

func TestBuildScopeGuardPrompt_TruncatesHugeDiff(t *testing.T) {
	huge := strings.Repeat("x", maxJudgeDiffChars+5000)
	msgs := buildScopeGuardPrompt("task", huge)
	if !strings.Contains(msgs[1].Content, "[diff truncated for scope review]") {
		t.Fatal("an oversized diff must be truncated with a marker")
	}
	if len(msgs[1].Content) > maxJudgeDiffChars+1024 {
		t.Fatalf("truncated user turn still too large: %d", len(msgs[1].Content))
	}
}
