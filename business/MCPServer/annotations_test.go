package business

// Two properties here, and both are security properties rather than cosmetics.
//
//  1. What we ADVERTISE is generated from what we ENFORCE, so the two cannot drift.
//     MCP annotations are hints a client may act on — notably by auto-retrying a
//     tool advertised as idempotent — so an annotation is a promise, not a label.
//  2. A delegation chain declared by an external caller is never used to decide
//     anything. If an inbound "hop: 0" were trusted, any caller could reset its own
//     budget by asserting one, and the budget would protect nothing.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAnnotationsAreGeneratedFromBehaviour(t *testing.T) {
	cases := []struct {
		name string
		b    ToolBehaviour
		want Annotations
	}{
		{
			"read-only tool is idempotent by nature",
			ToolBehaviour{ReadOnly: true},
			Annotations{ReadOnlyHint: true, DestructiveHint: false, IdempotentHint: true},
		},
		{
			"additive write is neither destructive nor idempotent",
			ToolBehaviour{},
			Annotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: false},
		},
		{
			"declared idempotent write",
			ToolBehaviour{Idempotent: true},
			Annotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: true},
		},
		{
			"destructive write",
			ToolBehaviour{Destructive: true},
			Annotations{ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := validSpec("t")
			spec.Behaviour = c.b
			got := AnnotationsFor(spec)
			got.Title = "" // compared separately; it is just the name
			if got != c.want {
				t.Errorf("AnnotationsFor(%+v) = %+v, want %+v", c.b, got, c.want)
			}
		})
	}
}

// A contradiction must never reach a client, even if it somehow reached a spec.
// Generating from behaviour rather than copying declared fields is what guarantees
// that, so the guarantee is worth asserting.
func TestAnnotationsNeverAdvertiseAReadOnlyDestructiveTool(t *testing.T) {
	spec := validSpec("contradiction")
	spec.Behaviour = ToolBehaviour{ReadOnly: true, Destructive: true}

	got := AnnotationsFor(spec)
	if got.ReadOnlyHint && got.DestructiveHint {
		t.Fatal("emitted a tool that is both read-only and destructive; a client " +
			"cannot act sensibly on that and would be right not to trust us")
	}
}

func TestAnnotationsForNilSpecIsSafe(t *testing.T) {
	if got := AnnotationsFor(nil); got != (Annotations{}) {
		t.Errorf("a nil spec must yield the zero annotation set, got %+v", got)
	}
}

// The escalation this prevents: a caller asserting a low hop count to win back a
// budget it has already spent outside our trust domain.
func TestEffectiveDepthIgnoresWhatTheCallerClaims(t *testing.T) {
	for _, claimed := range []int{0, 1, 5, -3, 1 << 20} {
		got := EffectiveDepth(DeclaredChain{Hop: claimed, Actors: []string{"a", "b", "c"}})
		if got != EntryDepth {
			t.Errorf("a caller claiming hop %d produced depth %d; depth must come from "+
				"our own credential, never from an assertion we cannot verify",
				claimed, got)
		}
	}
}

func TestEntryDepthIsNotZero(t *testing.T) {
	// An inbound MCP call is already a delegated act. Starting at zero would hand it
	// the full internal budget on top of whatever it spent upstream.
	if EntryDepth < 1 {
		t.Fatalf("EntryDepth = %d; an external call must not start with a fresh budget", EntryDepth)
	}
}

func TestSanitizeDeclaredActorsBoundsUntrustedInput(t *testing.T) {
	// Unbounded external input reaching the audit chain is an amplification vector
	// into the one table that has to stay readable and verifiable.
	many := make([]string, 100)
	for i := range many {
		many[i] = "actor"
	}
	if got := SanitizeDeclaredActors(many); len(got) > 16 {
		t.Errorf("actor list not bounded: %d entries", len(got))
	}

	if got := SanitizeDeclaredActors([]string{"", "   ", "real"}); len(got) != 1 || got[0] != "real" {
		t.Errorf("blank actors must be dropped, got %q", got)
	}

	// Rune-safe truncation: the audit hash is computed over the content, so invalid
	// UTF-8 written there is permanent.
	long := strings.Repeat("日", 500)
	got := SanitizeDeclaredActors([]string{long})
	if len(got) != 1 {
		t.Fatalf("expected one actor, got %d", len(got))
	}
	if !utf8.ValidString(got[0]) {
		t.Error("truncation produced invalid UTF-8, which would be written into the hash chain")
	}
	if n := utf8.RuneCountInString(got[0]); n > 128 {
		t.Errorf("actor id not bounded in characters: %d", n)
	}
}
