package ai

import (
	"context"
	"testing"
)

// The generic budget-dimension mechanism must stack composably, carry the right
// scope/limit, and ignore blank ids — so agent + channel caps layer cleanly
// onto the workspace/user meters without new plumbing.
func TestBudgetDimensionsStack(t *testing.T) {
	ctx := context.Background()
	if dims := budgetDimensions(ctx); len(dims) != 0 {
		t.Fatalf("plain context should have no dimensions, got %d", len(dims))
	}

	ctx = WithAgentBudget(ctx, "agent-1", 5000)
	ctx = WithChannelBudget(ctx, "chan-1", 0) // 0 = meter only, no cap

	dims := budgetDimensions(ctx)
	if len(dims) != 2 {
		t.Fatalf("expected 2 dimensions, got %d", len(dims))
	}
	if dims[0].Scope != BudgetScopeAgent || dims[0].ID != "agent-1" || dims[0].Limit != 5000 {
		t.Fatalf("agent dimension wrong: %+v", dims[0])
	}
	if dims[1].Scope != BudgetScopeChannel || dims[1].ID != "chan-1" || dims[1].Limit != 0 {
		t.Fatalf("channel dimension wrong: %+v", dims[1])
	}
}

func TestBudgetDimensionBlankIDIsNoop(t *testing.T) {
	ctx := WithAgentBudget(context.Background(), "", 1000)
	if dims := budgetDimensions(ctx); len(dims) != 0 {
		t.Fatalf("blank id should add no dimension, got %d", len(dims))
	}
}

// WithBudgetDimension must not mutate a parent context's dimension slice when a
// child layers another (no aliasing), so concurrent runs don't cross-pollute.
func TestBudgetDimensionsNoAliasing(t *testing.T) {
	parent := WithAgentBudget(context.Background(), "agent-1", 100)
	_ = WithChannelBudget(parent, "chan-1", 200)
	if dims := budgetDimensions(parent); len(dims) != 1 {
		t.Fatalf("parent must keep 1 dimension after child stacks, got %d", len(dims))
	}
}

// ChannelBudgetID must surface the channel scope threaded onto the run context
// (so the code agent can reuse it to gather the originating discussion), and be
// blank when no channel dimension is present.
func TestChannelBudgetID(t *testing.T) {
	if id := ChannelBudgetID(context.Background()); id != "" {
		t.Fatalf("plain context should have no channel id, got %q", id)
	}
	// Only an agent dimension → still no channel id.
	agentOnly := WithAgentBudget(context.Background(), "agent-1", 100)
	if id := ChannelBudgetID(agentOnly); id != "" {
		t.Fatalf("agent-only context should have no channel id, got %q", id)
	}
	// Channel dimension present → return its id.
	ctx := WithChannelBudget(agentOnly, "chan-42", 0)
	if id := ChannelBudgetID(ctx); id != "chan-42" {
		t.Fatalf("expected channel id chan-42, got %q", id)
	}
}

// AgentBudgetID must surface the agent scope threaded onto the run context (so a
// tool executor can recover the acting agent), and be blank when absent.
func TestAgentBudgetID(t *testing.T) {
	if id := AgentBudgetID(context.Background()); id != "" {
		t.Fatalf("plain context should have no agent id, got %q", id)
	}
	chOnly := WithChannelBudget(context.Background(), "chan-1", 0)
	if id := AgentBudgetID(chOnly); id != "" {
		t.Fatalf("channel-only context should have no agent id, got %q", id)
	}
	ctx := WithAgentBudget(chOnly, "agent-9", 100)
	if id := AgentBudgetID(ctx); id != "agent-9" {
		t.Fatalf("expected agent id agent-9, got %q", id)
	}
}

// GuardTokenBudget must fail-open (nil) when no cap is configured, so exposing
// it as an early check for heavy features never breaks the no-limit default.
func TestGuardTokenBudgetFailsOpenWhenUnconfigured(t *testing.T) {
	// No config set and no dimensions on the context → no cap can be exceeded.
	if err := GuardTokenBudget(context.Background()); err != nil {
		t.Fatalf("expected nil (fail-open) with no cap configured, got %v", err)
	}
	// A metering-only dimension (Limit 0) must never trip the guard either.
	ctx := WithChannelBudget(context.Background(), "chan-1", 0)
	if err := GuardTokenBudget(ctx); err != nil {
		t.Fatalf("meter-only channel dimension must not trip the guard, got %v", err)
	}
}
