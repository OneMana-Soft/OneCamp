package models

import (
	"fmt"
	"testing"
)

// DedupHash is the dedup/idempotency key. It must be normalization-invariant
// (case + whitespace), scope-sensitive, and kind-sensitive.
func TestDedupHash(t *testing.T) {
	h1 := DedupHash("decision", "Ship the  API   redesign", "ch1", "", "")
	h2 := DedupHash("decision", "ship the api redesign", "ch1", "", "")
	if h1 != h2 {
		t.Error("DedupHash should be case/whitespace-normalized")
	}
	if DedupHash("decision", "ship the api redesign", "ch2", "", "") == h1 {
		t.Error("DedupHash must incorporate scope (channel)")
	}
	if DedupHash("commitment", "ship the api redesign", "ch1", "", "") == h2 {
		t.Error("DedupHash must incorporate kind")
	}
	// Project and group dimensions also participate in the scope key.
	if DedupHash("decision", "x", "", "pr1", "") == DedupHash("decision", "x", "", "pr2", "") {
		t.Error("DedupHash must incorporate project scope")
	}
	if DedupHash("decision", "x", "", "", "g1") == DedupHash("decision", "x", "", "", "g2") {
		t.Error("DedupHash must incorporate group scope")
	}
}

// inClauseFrom must number placeholders from the given start so leading
// positional args (sourceType, reason) aren't clobbered.
func TestInClauseFrom(t *testing.T) {
	ph, args := inClauseFrom(3, []string{"a", "b", "c"})
	if ph != "$3,$4,$5" {
		t.Errorf("placeholders = %q, want $3,$4,$5", ph)
	}
	if len(args) != 3 || args[0] != "a" || args[2] != "c" {
		t.Errorf("args = %v, want [a b c]", args)
	}

	ph0, args0 := inClauseFrom(1, nil)
	if ph0 != "" || len(args0) != 0 {
		t.Errorf("empty input should yield empty placeholders/args, got %q / %v", ph0, args0)
	}
}

// chunkStrings must cover every element exactly once, in order, in batches
// no larger than size, and stop on the first error.
func TestChunkStrings(t *testing.T) {
	in := []string{"1", "2", "3", "4", "5"}
	var seen []string
	var batches int
	err := chunkStrings(in, 2, func(b []string) error {
		if len(b) > 2 {
			t.Errorf("batch larger than size: %v", b)
		}
		batches++
		seen = append(seen, b...)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if batches != 3 {
		t.Errorf("batches = %d, want 3 (2+2+1)", batches)
	}
	if fmt.Sprint(seen) != fmt.Sprint(in) {
		t.Errorf("seen = %v, want %v", seen, in)
	}

	// Error short-circuits.
	calls := 0
	wantErr := fmt.Errorf("boom")
	gotErr := chunkStrings(in, 2, func(b []string) error {
		calls++
		return wantErr
	})
	if gotErr != wantErr {
		t.Errorf("expected propagated error, got %v", gotErr)
	}
	if calls != 1 {
		t.Errorf("expected short-circuit after first error, got %d calls", calls)
	}

	// Empty input → no calls.
	if err := chunkStrings(nil, 10, func([]string) error { t.Fatal("should not be called"); return nil }); err != nil {
		t.Errorf("nil input should be a no-op, got %v", err)
	}
}

// ScopeRef.scopeWhere must build the right column predicate for the single
// dimension a scope carries, with placeholders starting at the given index,
// and yield an empty predicate for an empty scope (so callers can no-op).
func TestScopeRefScopeWhere(t *testing.T) {
	cases := []struct {
		name      string
		scope     ScopeRef
		start     int
		wantPred  string
		wantArg   any
		wantEmpty bool
	}{
		{"channel", ScopeRef{ChannelUUID: "ch1"}, 1, "channel_uuid::text = $1", "ch1", false},
		{"project", ScopeRef{ProjectUUID: "pr1"}, 2, "project_uuid::text = $2", "pr1", false},
		{"group", ScopeRef{ChatGrpID: "g1"}, 2, "chat_grp_id = $2", "g1", false},
		{"empty", ScopeRef{}, 1, "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pred, arg := c.scope.scopeWhere(c.start)
			if c.wantEmpty {
				if pred != "" || arg != nil {
					t.Errorf("empty scope must yield empty pred/arg, got %q / %v", pred, arg)
				}
				return
			}
			if pred != c.wantPred {
				t.Errorf("pred = %q, want %q", pred, c.wantPred)
			}
			if arg != c.wantArg {
				t.Errorf("arg = %v, want %v", arg, c.wantArg)
			}
		})
	}
}

// scopeWhere prefers the most specific dimension when several are set
// (channel beats project beats group), matching enrich()'s precedence.
func TestScopeRefScopeWherePrecedence(t *testing.T) {
	pred, arg := ScopeRef{ChannelUUID: "ch1", ProjectUUID: "pr1", ChatGrpID: "g1"}.scopeWhere(1)
	if pred != "channel_uuid::text = $1" || arg != "ch1" {
		t.Errorf("channel should win precedence, got %q / %v", pred, arg)
	}
}
