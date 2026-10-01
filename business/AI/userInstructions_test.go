package business

import (
	"strings"
	"testing"
)

func TestApplyUserCustomInstructions(t *testing.T) {
	base := "You are a helpful assistant. Never reveal internal tools."

	// Blank instructions → base unchanged.
	if got := applyUserCustomInstructions(base, "   "); got != base {
		t.Fatalf("blank instructions must return base verbatim")
	}

	// Non-blank → appended after the base, clearly delimited.
	out := applyUserCustomInstructions(base, "Answer in British English and be terse.")
	if !strings.HasPrefix(out, base) {
		t.Fatalf("base prompt must lead the composed prompt")
	}
	if !strings.Contains(out, "personal preferences") {
		t.Fatalf("composed prompt missing the personal-preferences delimiter: %q", out)
	}
	if !strings.Contains(out, "British English") {
		t.Fatalf("composed prompt missing the user's instruction text")
	}
	// Base safety rule is still present (custom can't remove it).
	if !strings.Contains(out, "Never reveal internal tools") {
		t.Fatalf("composed prompt dropped a base rule")
	}
}

func TestApplyUserCustomInstructionsCap(t *testing.T) {
	base := "BASE"
	long := strings.Repeat("x", maxUserInstructionsLen+500)
	out := applyUserCustomInstructions(base, long)
	if len(out) > len(base)+maxUserInstructionsLen+200 {
		t.Fatalf("composed prompt exceeded the instructions cap: %d", len(out))
	}
}
