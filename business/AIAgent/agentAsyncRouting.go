package business

// Sync-vs-durable routing decision for agent mentions (async-mentions spec
// Task 5.2). This is the ONE place that decides whether a mention runs
// synchronously (today's instant reply) or as a durable, resumable,
// progress-reporting job. Kept PURE (a function over the agent's config + the
// run's signals) so routing is unit-testable and can never silently make the
// common case worse: with no opt-in and no hand-off signal it returns false, so
// the fast path is unchanged.
//
// Two decision points share this function via AsyncSignal:
//   - at enqueue time (before a run): only OptIn is known.
//   - after a synchronous run hit a hard bound with unfinished work: the
//     StopReason + ToolsSucceeded + HandoffEnabled fields drive a hand-off to a
//     durable continuation instead of returning a truncated answer.

import (
	"os"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// AsyncSignal carries everything the routing decision needs. Zero value =>
// synchronous (unchanged behaviour).
type AsyncSignal struct {
	// OptIn is the per-agent "run my tasks in the background" flag
	// (ai_agents.run_in_background). When set, a mention run is durable from the
	// start.
	OptIn bool
	// HandoffEnabled mirrors AI_AGENT_ASYNC_HANDOFF: allow a bounded synchronous
	// run to hand off to a durable continuation. Cautious default OFF.
	HandoffEnabled bool
	// StopReason is a completed synchronous run's stable stop code (empty at
	// enqueue time). Only the "bounded, more work remained" codes trigger a
	// hand-off.
	StopReason string
	// ToolsSucceeded is how many tools actually executed in the synchronous run.
	// A hand-off only makes sense when the run had made real progress (else a
	// fresh durable run would just repeat the same bounded attempt).
	ToolsSucceeded int
}

// shouldRunAsync reports whether a mention run should take the durable path.
// Pure. Rules (v1): an explicit per-agent opt-in always goes durable; otherwise,
// when hand-off is enabled, a synchronous run that hit a hard bound
// (step/token/time limit) WITH real tool progress hands off to a durable
// continuation. Everything else stays synchronous (fast path unchanged).
func shouldRunAsync(sig AsyncSignal) bool {
	if sig.OptIn {
		return true
	}
	if sig.HandoffEnabled && sig.ToolsSucceeded > 0 && isBoundedStop(sig.StopReason) {
		return true
	}
	return false
}

// shouldRunMentionDurably decides whether a channel/thread @mention runs as a
// DURABLE job instead of one synchronous pass. Pure, so the policy is testable
// without a DB or a model.
//
// Why this matters beyond durability: every control a person has over an agent
// at work — stop it, correct it mid-run, watch its progress, resume it after it
// blocks on a question — exists only for a durable job. A synchronous mention
// run is a black box for up to the whole run timeout: it cannot be stopped, a
// reply in the thread cannot reach it (it queues a SECOND run instead), and the
// person sees nothing but a typing indicator. So any mention that could
// plausibly take real time — i.e. one where the agent has tools to use — is
// better off durable.
//
// A tool-less agent (pure conversation) stays synchronous: it answers in one
// model call, there is nothing to stop or steer, and the durable path would only
// add a queue hop.
//
// Rules:
//   - an explicit per-agent opt-in (run_in_background) always goes durable;
//   - otherwise, durable when the deployment allows it AND the agent has tools;
//   - everything else stays synchronous.
func shouldRunMentionDurably(optIn, hasTools, deploymentAllows bool) bool {
	if optIn {
		return true
	}
	return deploymentAllows && hasTools
}

// durableMentionsEnabled is the deployment-level switch for routing tool-capable
// @mentions through the durable engine (AI_AGENT_DURABLE_MENTIONS). ON by
// default: the controls a durable run provides (stop, mid-run steering, live
// progress, resume) are what make an agent acting on real data safe to leave
// running. Set it false to fall back to the previous synchronous behaviour.
func durableMentionsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AI_AGENT_DURABLE_MENTIONS"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// isBoundedStop reports whether a stop code means "the run was cut off with more
// work possible" (as opposed to a clean finish, a block, or a budget cap that
// awaiting more compute won't fix within the same day).
func isBoundedStop(stopReason string) bool {
	switch stopReason {
	case StopReasonStepLimit, StopReasonRunTokenLimit, StopReasonRunTimeout:
		return true
	default:
		return false
	}
}

// asyncHandoffEnabled reads the cautious env gate for the sync->durable hand-off
// (Task 6). Default OFF. The per-agent OptIn path does not depend on this.
func asyncHandoffEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_AGENT_ASYNC_HANDOFF")))
	return v == "true" || v == "1" || v == "on" || v == "yes"
}

// agentEnabledTools decodes an agent's tool allow-list from its stored JSON.
// One decoder for every caller (the runner, the routing decision, anything
// later) so "does this agent have tools" can never disagree between them. A
// blank or malformed blob yields no tools, which is the safe reading.
//
// The decoder moved to the model because the MCP surface needs the same answer for
// a credential bound to this agent, and that package must not import this one. This
// stays as the name the runner already reads well with.
func agentEnabledTools(a *model.AiAgent) []string {
	return a.EnabledToolList()
}

// agentHasTools reports whether the agent can act on the workspace at all (as
// opposed to a conversation-only agent).
func agentHasTools(a *model.AiAgent) bool {
	return len(agentEnabledTools(a)) > 0
}
