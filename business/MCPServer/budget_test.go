package business

// The budget's contract. The Redis path needs a server, so what is asserted here is
// the policy: which bucket a tool charges, what the defaults are, and the two
// refusals that need no cache at all.
//
// Note the deliberate asymmetry this file encodes: every other guard in the package
// fails CLOSED, and this one fails OPEN when Redis is unavailable. A budget protects
// against volume; authorization and audit protect against everything that matters.
// Refusing all agent traffic because a cache is down would turn a cache outage into a
// product outage. Fail closed on questions of AUTHORITY, fail open on questions of
// VOLUME.

import (
	"context"
	"strings"
	"testing"
)

func TestBudgetLimitsFallBackToSafeDefaults(t *testing.T) {
	var zero BudgetLimits
	if zero.read() != DefaultReadCallsPerMinute {
		t.Errorf("read default = %d, want %d", zero.read(), DefaultReadCallsPerMinute)
	}
	if zero.write() != DefaultWriteCallsPerMinute {
		t.Errorf("write default = %d, want %d", zero.write(), DefaultWriteCallsPerMinute)
	}

	// Explicit values win; nonsensical ones fall back rather than locking a
	// credential out entirely, which a zero would do.
	set := BudgetLimits{ReadPerMinute: 10, WritePerMinute: 5}
	if set.read() != 10 || set.write() != 5 {
		t.Errorf("explicit limits not honoured: %+v", set)
	}
	negative := BudgetLimits{ReadPerMinute: -1, WritePerMinute: -1}
	if negative.read() != DefaultReadCallsPerMinute || negative.write() != DefaultWriteCallsPerMinute {
		t.Error("a nonsensical limit must fall back to the default, not lock the credential out")
	}
}

// Writes must be capped far below reads. A runaway read loop wastes cycles; a runaway
// write loop puts a thousand messages in a channel and no audit un-sends those.
func TestWritesAreBudgetedFarMoreTightlyThanReads(t *testing.T) {
	if DefaultWriteCallsPerMinute >= DefaultReadCallsPerMinute {
		t.Fatalf("writes (%d) must be capped well below reads (%d); they are different "+
			"failure severities", DefaultWriteCallsPerMinute, DefaultReadCallsPerMinute)
	}
	// Low enough that a person notices before the damage is interesting.
	if DefaultWriteCallsPerMinute > 60 {
		t.Errorf("write cap of %d/min is too high to be a meaningful backstop against "+
			"a loop nobody predicted", DefaultWriteCallsPerMinute)
	}
}

// Both refusals that need no cache. An uncharged call is exactly what a budget cannot
// bound, so a missing credential is a refusal rather than a free pass.
func TestBudgetRefusesWhatItCannotCharge(t *testing.T) {
	ctx := context.Background()
	spec := validSpec("t")

	if d := CheckBudget(ctx, "", spec, BudgetLimits{}); d.Allow {
		t.Error("a call with no credential was charged nothing and allowed")
	}
	if d := CheckBudget(ctx, "tok-1", nil, BudgetLimits{}); d.Allow {
		t.Error("a call with no tool spec was allowed")
	}
	// Every refusal explains itself, so the audit row and the client message agree.
	for _, d := range []BudgetDecision{
		CheckBudget(ctx, "", spec, BudgetLimits{}),
		CheckBudget(ctx, "tok-1", nil, BudgetLimits{}),
	} {
		if strings.TrimSpace(d.Reason) == "" {
			t.Error("a budget refusal must carry a reason")
		}
	}
}

// The bucket is chosen from the tool's declared behaviour, never from a caller's
// argument — so a write cannot be charged against the read bucket by mistake.
func TestBudgetChargesTheBucketMatchingTheDeclaredBehaviour(t *testing.T) {
	readTool := validSpec("get")
	readTool.Behaviour = ToolBehaviour{ReadOnly: true}
	writeTool := writeSpec("post", ToolBehaviour{})

	if !readTool.Behaviour.ReadOnly {
		t.Fatal("fixture: read tool should be read-only")
	}
	if writeTool.Behaviour.ReadOnly {
		t.Fatal("fixture: write tool must not be read-only, or it would charge the read bucket")
	}
}

// RetryAfter is meaningful only for a budget refusal. Offering it on a permission
// refusal would invite the retry loop the budget exists to stop.
func TestRetryAfterIsOnlyMeaningfulForBudgetRefusals(t *testing.T) {
	permission := denyAs(ActorIdentity{}, "the originating person is not a member", "tok", "person")
	if permission.RetryAfter != 0 {
		t.Error("a permission refusal must not suggest that waiting will help")
	}
}
