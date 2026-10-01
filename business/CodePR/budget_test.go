package codepr

import (
	"strings"
	"testing"
)

func TestCheckBudget_AllUnlimited(t *testing.T) {
	// Zero caps everywhere => never blocks, regardless of usage.
	in := BudgetInput{
		AgentUsage:     TierUsage{Minutes: 9999, Runs: 9999},
		ChannelUsage:   TierUsage{Minutes: 9999, Runs: 9999},
		WorkspaceUsage: TierUsage{Minutes: 9999, Runs: 9999},
	}
	if d := CheckBudget(in); !d.Allowed {
		t.Fatalf("unlimited tiers must allow, got %+v", d)
	}
}

func TestCheckBudget_UnderCapAllowed(t *testing.T) {
	in := BudgetInput{
		AgentCap:     TierBudget{Minutes: 60, Runs: 10},
		AgentUsage:   TierUsage{Minutes: 30, Runs: 4},
		WorkspaceCap: TierBudget{Minutes: 600, Runs: 100},
	}
	if d := CheckBudget(in); !d.Allowed {
		t.Fatalf("under cap must allow, got %+v", d)
	}
}

func TestCheckBudget_AgentRunsExhausted(t *testing.T) {
	in := BudgetInput{
		AgentCap:   TierBudget{Runs: 5},
		AgentUsage: TierUsage{Runs: 5}, // reached the cap
	}
	d := CheckBudget(in)
	if d.Allowed || d.StopReason != StopReasonCodingBudgetAgent {
		t.Fatalf("agent run cap should block with agent reason, got %+v", d)
	}
	if !strings.Contains(d.Message, "run limit") {
		t.Fatalf("message should mention the run limit: %q", d.Message)
	}
}

func TestCheckBudget_AgentMinutesExhausted(t *testing.T) {
	in := BudgetInput{
		AgentCap:   TierBudget{Minutes: 45},
		AgentUsage: TierUsage{Minutes: 50}, // over
	}
	d := CheckBudget(in)
	if d.Allowed || d.StopReason != StopReasonCodingBudgetAgent {
		t.Fatalf("agent minute cap should block, got %+v", d)
	}
	if !strings.Contains(d.Message, "time limit") {
		t.Fatalf("message should mention the time limit: %q", d.Message)
	}
}

func TestCheckBudget_TightestScopeFirst(t *testing.T) {
	// Agent clear, channel exhausted, workspace exhausted → channel wins (more
	// specific than workspace).
	in := BudgetInput{
		AgentCap:       TierBudget{Runs: 100},
		AgentUsage:     TierUsage{Runs: 1},
		ChannelCap:     TierBudget{Runs: 3},
		ChannelUsage:   TierUsage{Runs: 3},
		WorkspaceCap:   TierBudget{Runs: 10},
		WorkspaceUsage: TierUsage{Runs: 10},
	}
	d := CheckBudget(in)
	if d.Allowed || d.StopReason != StopReasonCodingBudgetChannel {
		t.Fatalf("channel cap should win over workspace, got %+v", d)
	}
}

func TestCheckBudget_WorkspaceExhausted(t *testing.T) {
	in := BudgetInput{
		WorkspaceCap:   TierBudget{Minutes: 600},
		WorkspaceUsage: TierUsage{Minutes: 600},
	}
	d := CheckBudget(in)
	if d.Allowed || d.StopReason != StopReasonCodingBudgetWorkspace {
		t.Fatalf("workspace cap should block, got %+v", d)
	}
}

func TestCheckBudget_ZeroDimensionUnlimitedWithinTier(t *testing.T) {
	// A tier with a run cap but no minute cap must only block on runs.
	in := BudgetInput{
		AgentCap:   TierBudget{Runs: 5, Minutes: 0},
		AgentUsage: TierUsage{Runs: 2, Minutes: 100000},
	}
	if d := CheckBudget(in); !d.Allowed {
		t.Fatalf("minutes uncapped should not block on minutes, got %+v", d)
	}
}
