package codepr

// Coding budget gate — the pure decision that refuses a coding run when any tier
// (agent / channel / workspace) has exhausted its daily wall-clock-minutes or
// run-count cap. It mirrors the token-budget + sandbox-budget tiers, returns a
// STABLE typed stop-reason (not an error), and treats a 0 cap as unlimited. The
// caller (orchestrator) sources caps from admin config + the agent row and usage
// from the code_pr_runs ledger, then gates BEFORE minting a token or dispatching
// to the runner. Pure + fully unit-testable.

// Stop-reason codes for a refused coding run. Durable callers switch on these
// to surface the right message ("daily coding budget reached — try tomorrow")
// and to classify the stop as a clean pause, not a failure.
const (
	StopReasonCodingBudget          = "coding_budget"           // a tier cap is exhausted (generic)
	StopReasonCodingBudgetAgent     = "coding_budget_agent"     // this agent's daily cap
	StopReasonCodingBudgetChannel   = "coding_budget_channel"   // this channel's daily cap
	StopReasonCodingBudgetWorkspace = "coding_budget_workspace" // the workspace daily cap
)

// TierBudget is one tier's daily caps. 0 (or negative) means "no cap" for that
// dimension, so a tier with both zero never blocks.
type TierBudget struct {
	Minutes int
	Runs    int
}

// TierUsage is one tier's consumption so far today.
type TierUsage struct {
	Minutes int
	Runs    int
}

// BudgetInput bundles the caps + usage for all three tiers. A tier whose caps
// are both 0 is unlimited and never consulted. Any tier may be omitted (zero
// value) when it does not apply (e.g. an assistant run with no agent/channel).
type BudgetInput struct {
	AgentCap       TierBudget
	AgentUsage     TierUsage
	ChannelCap     TierBudget
	ChannelUsage   TierUsage
	WorkspaceCap   TierBudget
	WorkspaceUsage TierUsage
}

// BudgetDecision is the outcome of the gate. Allowed=false carries the specific
// exhausted tier's stop-reason and a human-readable message.
type BudgetDecision struct {
	Allowed    bool
	StopReason string
	Message    string
}

// CheckBudget decides whether a new coding run may start. It checks the tightest
// scope first (agent → channel → workspace) so the message names the most
// specific limit the requester can reason about, and refuses on the FIRST
// exhausted dimension. A run counts as "one more run" and is expected to consume
// at least a minute, so a tier is exhausted when usage has REACHED the cap
// (>=), never only when it would exceed it — this prevents a run starting with
// no budget left. Pure.
func CheckBudget(in BudgetInput) BudgetDecision {
	if d, blocked := checkTier(in.AgentCap, in.AgentUsage, StopReasonCodingBudgetAgent, "this agent's"); blocked {
		return d
	}
	if d, blocked := checkTier(in.ChannelCap, in.ChannelUsage, StopReasonCodingBudgetChannel, "this channel's"); blocked {
		return d
	}
	if d, blocked := checkTier(in.WorkspaceCap, in.WorkspaceUsage, StopReasonCodingBudgetWorkspace, "the workspace's"); blocked {
		return d
	}
	return BudgetDecision{Allowed: true}
}

// checkTier reports whether one tier is exhausted (returning the refusal), or
// clear. A 0/negative cap for a dimension means unlimited for that dimension.
func checkTier(cap TierBudget, usage TierUsage, reason, who string) (BudgetDecision, bool) {
	if cap.Runs > 0 && usage.Runs >= cap.Runs {
		return BudgetDecision{
			Allowed:    false,
			StopReason: reason,
			Message:    "Reached " + who + " daily coding-run limit. It resets tomorrow (UTC).",
		}, true
	}
	if cap.Minutes > 0 && usage.Minutes >= cap.Minutes {
		return BudgetDecision{
			Allowed:    false,
			StopReason: reason,
			Message:    "Reached " + who + " daily coding-time limit. It resets tomorrow (UTC).",
		}, true
	}
	return BudgetDecision{}, false
}
