package ai

import (
	"context"
	"testing"
)

// WithSandboxScope/SandboxScopeFromContext must round-trip a populated scope,
// treat a zero-value scope as a no-op (so callers can tag unconditionally), and
// return the zero value for an untagged (assistant-path) context.
func TestSandboxScopeRoundTrip(t *testing.T) {
	base := context.Background()

	// Untagged context → zero value.
	if got := SandboxScopeFromContext(base); got != (SandboxScope{}) {
		t.Fatalf("untagged ctx: want zero scope, got %+v", got)
	}

	// Zero-value scope is a no-op: it must not shadow a parent value and must
	// still read back as zero.
	if ctx := WithSandboxScope(base, SandboxScope{}); SandboxScopeFromContext(ctx) != (SandboxScope{}) {
		t.Fatalf("zero scope should read back as zero")
	}

	want := SandboxScope{
		AgentID:           "agent-1",
		ChannelID:         "chan-9",
		RunID:             "run-7",
		AgentDailySeconds: 120,
		AgentDailyRuns:    5,
	}
	ctx := WithSandboxScope(base, want)
	if got := SandboxScopeFromContext(ctx); got != want {
		t.Fatalf("round-trip mismatch: want %+v, got %+v", want, got)
	}

	// A nil context must not panic and must report the zero value.
	if got := SandboxScopeFromContext(nil); got != (SandboxScope{}) { //nolint:staticcheck
		t.Fatalf("nil ctx: want zero scope, got %+v", got)
	}
}
