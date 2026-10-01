package codesandbox

import "testing"

func agentTier(caps Caps, usage TierUsage) Tier {
	return Tier{Reason: StopReasonAgentBudget, Caps: caps, Usage: usage}
}
func wsTier(caps Caps, usage TierUsage) Tier {
	return Tier{Reason: StopReasonWorkspaceBudget, Caps: caps, Usage: usage}
}

func TestCheckBudget_DisabledMasterSwitch(t *testing.T) {
	d := CheckBudget(false, 5, nil)
	if d.Allowed || d.Reason != StopReasonDisabled {
		t.Fatalf("disabled sandbox must refuse with sandbox_disabled, got %+v", d)
	}
}

func TestCheckBudget_NoTiersAllows(t *testing.T) {
	if d := CheckBudget(true, 10, nil); !d.Allowed {
		t.Fatalf("enabled with no caps should allow, got %+v", d)
	}
}

func TestCheckBudget_UnlimitedTierNeverBlocks(t *testing.T) {
	d := CheckBudget(true, 100, []Tier{agentTier(Caps{}, TierUsage{Seconds: 10_000, Runs: 10_000})})
	if !d.Allowed {
		t.Fatalf("unlimited caps must never block, got %+v", d)
	}
}

func TestCheckBudget_RunCountCap(t *testing.T) {
	// 5 runs allowed, 5 already used → refuse.
	d := CheckBudget(true, 1, []Tier{agentTier(Caps{DailyRuns: 5}, TierUsage{Runs: 5})})
	if d.Allowed || d.Reason != StopReasonAgentBudget {
		t.Fatalf("run-count cap should block with agent reason, got %+v", d)
	}
	// 4 used → allowed.
	if d := CheckBudget(true, 1, []Tier{agentTier(Caps{DailyRuns: 5}, TierUsage{Runs: 4})}); !d.Allowed {
		t.Fatalf("under run cap should allow, got %+v", d)
	}
}

func TestCheckBudget_SecondsCapAndEstimate(t *testing.T) {
	caps := Caps{DailySeconds: 60}
	// 50 used, estimate 5 → 55 <= 60 → allow.
	if d := CheckBudget(true, 5, []Tier{wsTier(caps, TierUsage{Seconds: 50})}); !d.Allowed {
		t.Fatalf("estimate within remaining should allow, got %+v", d)
	}
	// 50 used, estimate 20 → 70 > 60 → refuse.
	if d := CheckBudget(true, 20, []Tier{wsTier(caps, TierUsage{Seconds: 50})}); d.Allowed || d.Reason != StopReasonWorkspaceBudget {
		t.Fatalf("estimate over remaining should block workspace, got %+v", d)
	}
	// Already at cap → refuse regardless of estimate.
	if d := CheckBudget(true, 0, []Tier{wsTier(caps, TierUsage{Seconds: 60})}); d.Allowed {
		t.Fatalf("at seconds cap should block, got %+v", d)
	}
}

func TestCheckBudget_UnknownEstimateChecksOnlyRuns(t *testing.T) {
	// estSeconds<=0 (unknown): seconds dimension with headroom must not block.
	caps := Caps{DailySeconds: 60, DailyRuns: 10}
	if d := CheckBudget(true, 0, []Tier{agentTier(caps, TierUsage{Seconds: 59, Runs: 1})}); !d.Allowed {
		t.Fatalf("unknown estimate with seconds headroom should allow, got %+v", d)
	}
	// But no seconds headroom still blocks even with unknown estimate.
	if d := CheckBudget(true, 0, []Tier{agentTier(caps, TierUsage{Seconds: 60, Runs: 1})}); d.Allowed {
		t.Fatalf("unknown estimate with zero headroom should block, got %+v", d)
	}
}

func TestCheckBudget_MostSpecificTierBlocksFirst(t *testing.T) {
	// Agent tier fine, workspace tier exhausted → evaluation order (agent
	// first, workspace second) means workspace reason is reported.
	tiers := []Tier{
		agentTier(Caps{DailyRuns: 100}, TierUsage{Runs: 1}),
		wsTier(Caps{DailyRuns: 10}, TierUsage{Runs: 10}),
	}
	if d := CheckBudget(true, 1, tiers); d.Allowed || d.Reason != StopReasonWorkspaceBudget {
		t.Fatalf("workspace exhaustion should block with workspace reason, got %+v", d)
	}
	// Agent exhausted → agent reason wins (listed first).
	tiers2 := []Tier{
		agentTier(Caps{DailyRuns: 5}, TierUsage{Runs: 5}),
		wsTier(Caps{DailyRuns: 10}, TierUsage{Runs: 1}),
	}
	if d := CheckBudget(true, 1, tiers2); d.Reason != StopReasonAgentBudget {
		t.Fatalf("agent exhaustion should win when listed first, got %+v", d)
	}
}
