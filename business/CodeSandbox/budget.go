package codesandbox

// budget.go — the pure decision logic that gates a sandbox run against
// admin-configured caps, before any resources are committed. It mirrors the
// four-tier structure of the existing AI token budgets (agent / channel /
// workspace) but is a total, side-effect-free function: the orchestrator
// gathers "today's usage" and the configured caps for each applicable tier and
// asks CheckBudget whether a new run may proceed and, if not, WHY (a stable,
// typed reason the caller surfaces like the other budget stop-reasons).
//
// Persistence of usage (the ledger) and the config plumbing live with the
// orchestrator/DB layer; keeping the decision pure makes every allow/deny path
// exhaustively unit-testable without a database.

// StopReason is a stable, machine-readable code for why a run was refused or
// could not proceed. Empty means "allowed".
type StopReason string

const (
	StopReasonNone            StopReason = ""
	StopReasonDisabled        StopReason = "sandbox_disabled"         // admin has not enabled the sandbox
	StopReasonUnavailable     StopReason = "sandbox_unavailable"      // runner down/misconfigured
	StopReasonBusy            StopReason = "sandbox_busy"             // concurrency queue full
	StopReasonAgentBudget     StopReason = "sandbox_agent_budget"     // per-agent daily cap hit
	StopReasonChannelBudget   StopReason = "sandbox_channel_budget"   // per-channel daily cap hit
	StopReasonWorkspaceBudget StopReason = "sandbox_workspace_budget" // workspace daily cap hit
	StopReasonUserBudget      StopReason = "sandbox_user_budget"      // per-user daily cap (assistant path)
)

// Caps is one tier's daily allowance. A zero field means "no limit on that
// dimension" (mirroring the token-budget convention where 0 = unlimited).
type Caps struct {
	DailySeconds int
	DailyRuns    int
}

// unlimited reports whether the caps impose no limit at all.
func (c Caps) unlimited() bool { return c.DailySeconds <= 0 && c.DailyRuns <= 0 }

// TierUsage is a tier's consumption so far today.
type TierUsage struct {
	Seconds int
	Runs    int
}

// Tier binds a tier's identity + its caps + today's usage for evaluation. Name
// selects the StopReason when this tier is the one that blocks.
type Tier struct {
	Reason StopReason // the stop-reason to report if THIS tier blocks
	Caps   Caps
	Usage  TierUsage
}

// Decision is the outcome of a budget check.
type Decision struct {
	Allowed bool
	Reason  StopReason // StopReasonNone when Allowed
}

// allow is the allowed decision.
var allow = Decision{Allowed: true, Reason: StopReasonNone}

// CheckBudget decides whether a new run may proceed. `enabled` is the admin
// master switch; `tiers` are the applicable caps+usage tiers (typically
// agent, channel, workspace — and user for the assistant path), evaluated in
// the given order so the most specific tier that blocks is reported first.
//
// A run of estimated `estSeconds` is admitted only if EVERY capped tier has
// room for both one more run and (best-effort) the estimated seconds. A tier
// with unlimited caps never blocks. estSeconds <= 0 is treated as "unknown"
// and only the run-count dimension is checked for that tier.
func CheckBudget(enabled bool, estSeconds int, tiers []Tier) Decision {
	if !enabled {
		return Decision{Allowed: false, Reason: StopReasonDisabled}
	}
	if estSeconds < 0 {
		estSeconds = 0
	}
	for _, t := range tiers {
		if t.Caps.unlimited() {
			continue
		}
		// Run-count dimension: refuse when the tier has already met/exceeded
		// its daily run allowance.
		if t.Caps.DailyRuns > 0 && t.Usage.Runs >= t.Caps.DailyRuns {
			return Decision{Allowed: false, Reason: t.Reason}
		}
		// Seconds dimension: refuse when the tier has no headroom left, or the
		// estimate would exceed the remaining budget. When already at/over the
		// cap, refuse regardless of the estimate.
		if t.Caps.DailySeconds > 0 {
			remaining := t.Caps.DailySeconds - t.Usage.Seconds
			if remaining <= 0 || estSeconds > remaining {
				return Decision{Allowed: false, Reason: t.Reason}
			}
		}
	}
	return allow
}
