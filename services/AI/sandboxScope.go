package ai

// sandboxScope carries the run identity + per-agent execution-sandbox caps from
// the agent runner (which knows the agent, channel, and run) down to the
// run_analysis executor (which lives in another package and otherwise has no
// way to attribute a run or enforce the per-agent/per-channel budget tiers).
//
// It is deliberately a plain, DB-free data tag — the same generic pattern as
// WithActor / WithAgentBudget — so services/AI stays free of business/model
// dependencies. Absent (the assistant path, a manual/scheduled run with no
// channel) → the executor simply enforces only the workspace tier and records
// the run against the acting user, which is always correct.

import "context"

// SandboxScope is the run context a sandbox execution is attributed to and
// budgeted against. Every field is optional; zero values mean "not applicable"
// (that tier is skipped, that id is left NULL in the audit row).
type SandboxScope struct {
	AgentID   string // the running agent (empty for the assistant path)
	ChannelID string // the conversation channel (empty for DM/group/manual)
	RunID     string // the ai_agent_runs row this belongs to (empty if none)
	// Per-agent daily caps copied from the agent row (0 = no per-agent cap).
	AgentDailySeconds int
	AgentDailyRuns    int
}

type sandboxScopeKeyT struct{}

var sandboxScopeKey sandboxScopeKeyT

// WithSandboxScope tags ctx with the sandbox run identity + per-agent caps. A
// zero-value scope is a no-op (keeps the parent ctx unchanged), so callers can
// pass through unconditionally.
func WithSandboxScope(ctx context.Context, s SandboxScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == (SandboxScope{}) {
		return ctx
	}
	return context.WithValue(ctx, sandboxScopeKey, s)
}

// SandboxScopeFromContext returns the sandbox scope tagged on ctx, or the zero
// value when none is set (assistant path / untagged caller).
func SandboxScopeFromContext(ctx context.Context) SandboxScope {
	if ctx == nil {
		return SandboxScope{}
	}
	if s, ok := ctx.Value(sandboxScopeKey).(SandboxScope); ok {
		return s
	}
	return SandboxScope{}
}
