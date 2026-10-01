package business

// The agent runner: the LLM tool-loop that executes one agent. It reuses the
// AI service's proven primitives — the tool registry (ai.Executors, which
// re-check the acting user's permissions), tool-call parsing, the per-model
// circuit breaker and rate limiter — so an agent can never do something its
// owner couldn't do by hand, and adds no parallel model/runtime machinery.
//
// Safety model:
//   - Runs AS the agent's owner (agent.CreatedBy); every tool call re-checks
//     that user's permissions inside the executor.
//   - May only call tools on the agent's allow-list; anything else is dropped.
//   - Bounded by the agent's max_steps (hard-capped) and the AI budget
//     (circuit breaker + rate limit), so a loop can't run away or run up cost.
//   - Writes are tagged automation-generated so they can't re-trigger workflows
//     or other agents (no cascade).
//   - Dry-run executes read-only tools (for a realistic preview) but never
//     writes.
//   - The full transcript (assistant turns + tool calls + results) is recorded
//     to ai_agent_runs for audit and replay.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// RunOutcome is the result of one agent run, returned to interactive callers
// (the builder's "test" action) and persisted to the run row.
type RunOutcome struct {
	RunID  uuid.UUID `json:"run_id"`
	Status string    `json:"status"`
	Result string    `json:"result"`
	Error  string    `json:"error,omitempty"`
	Steps  int       `json:"steps"`
	// Proposed lists the human-readable descriptions of writes an approval-mode
	// run queued for approval (instead of executing). Lets a caller disclose
	// "I've proposed N changes pending approval" deterministically, rather than
	// relying on the model to narrate it.
	Proposed []string `json:"proposed,omitempty"`
	// ToolsUsed lists the distinct tool names the run invoked (in call order),
	// regardless of whether each call executed, was skipped, or errored. Used by
	// the evaluation harness to assert on tool selection.
	ToolsUsed []string `json:"tools_used,omitempty"`
	// ToolsSucceeded lists the distinct tools that actually EXECUTED without an
	// error (writes that landed, reads that returned). This — not ToolsUsed — is
	// what a user-facing "Worked with: …" disclosure must use, so the comment
	// never claims work for a tool call that failed or was skipped. The model's
	// own prose summary is untrustworthy on weak models; this is ground truth.
	ToolsSucceeded []string `json:"tools_succeeded,omitempty"`
	// FailedTools lists the distinct tools whose calls returned an error during
	// the run. When non-empty on an otherwise "succeeded"/"stopped" run, the
	// agent's prose claimed success while real actions failed — a durable caller
	// surfaces this so a hallucinated "done" can't hide a no-op.
	FailedTools []string `json:"failed_tools,omitempty"`
	// FreshCalls counts calls that succeeded and had not been made before in
	// this conversation (a resumed session counts only its new ones). It is the
	// run's progress: a durable job that hits its step limit continues in a new
	// session only when the session that ended found or did something new.
	FreshCalls int `json:"fresh_calls,omitempty"`
	// Blocked is set when the run paused itself via the generic needs_human
	// blocker (it cannot proceed without a human decision). BlockReason carries
	// the question/blocker to surface. A blocked run is a clean stop, not a
	// failure: a durable caller parks it as awaiting_input and resumes it when a
	// human replies.
	Blocked     bool   `json:"blocked,omitempty"`
	BlockReason string `json:"block_reason,omitempty"`
	// BlockOptions are the answers the agent offered alongside the question,
	// already validated and bounded. Empty means the reply is free text.
	//
	// Carried on the outcome rather than re-parsed from the question text,
	// because the list a person is shown and the list the resume matches
	// against must be the same list. Deriving one from the other's prose is how
	// they drift.
	BlockOptions []string `json:"block_options,omitempty"`
	// StopReason is a STABLE, machine-readable code explaining why a RunStopped
	// outcome stopped (empty for non-stopped outcomes). Durable callers classify
	// the stop on this code rather than parsing the human-readable Error text,
	// so changing a message string can never silently break the task-state
	// machine (e.g. a budget pause being mistaken for a clean completion).
	StopReason string `json:"stop_reason,omitempty"`
}

// Stop-reason codes for a RunStopped outcome. A durable caller switches on
// these to choose the right transition (await + resume vs free retry vs
// finalize-with-partial), independent of the displayed message wording.
const (
	StopReasonBlocked         = "blocked"          // needs_human: await a human reply
	StopReasonCircuitOpen     = "circuit_open"     // model breaker open: free retry
	StopReasonRateLimited     = "rate_limited"     // rate limited: free retry
	StopReasonRunTimeout      = "run_timeout"      // wall-clock deadline: free retry
	StopReasonUserBudget      = "user_budget"      // owner daily cap: await reset
	StopReasonAgentBudget     = "agent_budget"     // agent daily cap: await reset
	StopReasonChannelBudget   = "channel_budget"   // channel daily cap: await reset
	StopReasonWorkspaceBudget = "workspace_budget" // workspace daily cap: await reset
	StopReasonStepLimit       = "step_limit"       // bounded completion: finalize partial
	StopReasonRunTokenLimit   = "run_token_limit"  // bounded completion: finalize partial
	// StopReasonCanceled: the run context was cancelled while the loop was
	// running — a human asked the job to stop, or this worker lost its lease.
	// Either way the run did not fail, so it must not be reported (or retried)
	// as a failure; the durable caller decides which of the two it was and
	// settles accordingly.
	StopReasonCanceled = "canceled"
)

// replyStyleRule is how every agent writes to people, whatever its sponsor's
// instructions say about the task. Seen live: a correct answer about launch
// items carried "(source: list_project_tasks)" on every line and a "Sources"
// block pasting the whole channel back as code. The reply already says which
// tools were used ("Worked with: ..."), so naming them again is noise, and an
// id means nothing to the person reading.
const replyStyleRule = "\n\nHow you write replies: for the people in the conversation, in plain sentences " +
	"and short lists. Never name your tools, internal ids or UUIDs, and never paste raw tool output or the " +
	"conversation back; say where something came from in words (\"from the Q4 launch project\") when it helps."

// blockerToolName is the generic, registry-free tool an agent emits to pause
// for a human decision (needs_human). Intercepted by the runner loop; it has no
// executor and is available to every agent regardless of its allow-list.
const blockerToolName = "needs_human"

// progressToolName is the generic, registry-free tool an agent emits to persist
// its continue-the-work state across runs (save_progress). Intercepted by the
// runner loop; it writes the agent's durable state blob and the loop continues
// (it is self-state, not an external write, so it always runs and is never
// approval-gated). Available to every agent.
const progressToolName = "save_progress"

// agentResumeKey carries durable-resume state into a run. ONLY the durable task
// worker sets it; every other caller (mention/DM/schedule/manual/test) leaves
// it unset, so their runs are byte-identical to before. When set, the runner
// continues the SAVED conversation (rebuilding its dedupe set from the prior
// tool calls so an already-performed write is never repeated) and checkpoints
// the message list after each step so a pause/crash resumes mid-conversation.
type agentResumeKey struct{}

type agentResumeState struct {
	messages   []ai.ChatMessage
	checkpoint func([]ai.ChatMessage)
}

// WithAgentResumeState attaches resume messages + a per-step checkpoint to ctx.
// messages may be nil (a fresh run that should still checkpoint); checkpoint may
// be nil (resume without persisting). Used only by the durable task worker.
func WithAgentResumeState(ctx context.Context, messages []ai.ChatMessage, checkpoint func([]ai.ChatMessage)) context.Context {
	return context.WithValue(ctx, agentResumeKey{}, &agentResumeState{messages: messages, checkpoint: checkpoint})
}

// agentSessionLeftKey marks a durable run whose job may carry on in another
// session if this one runs out of steps.
type agentSessionLeftKey struct{}

// WithAnotherSession tells the run that its job can continue in a new
// session. Set only by the durable worker.
func WithAnotherSession(ctx context.Context, left bool) context.Context {
	return context.WithValue(ctx, agentSessionLeftKey{}, left)
}

// skipBoundedSummary reports whether a run that hit a per-run bound can skip
// its closing summary: another session will continue the work (it made
// progress, and one is available), so nobody reads the summary and the call
// would only spend tokens re-reading the whole transcript.
func skipBoundedSummary(ctx context.Context, freshCalls int) bool {
	left, _ := ctx.Value(agentSessionLeftKey{}).(bool)
	return left && freshCalls > 0
}

func agentResumeFromCtx(ctx context.Context) *agentResumeState {
	if v, ok := ctx.Value(agentResumeKey{}).(*agentResumeState); ok {
		return v
	}
	return nil
}

// agentProgressKey carries a per-step progress callback into a run. ONLY the
// durable task worker sets it (to update a live "working…" status comment as
// stages complete); every other caller leaves it unset, so their runs are
// byte-identical. The callback is invoked after each completed step with the
// distinct tool names used so far (humanizable by the caller), so it can render
// "Working… (used: …)" without the runner knowing about comments.
type agentProgressKey struct{}

// WithAgentProgress attaches a per-step progress callback (worker-only). fn is
// called after each step with the distinct tool names used so far (call order).
func WithAgentProgress(ctx context.Context, fn func(toolsUsed []string)) context.Context {
	if ctx == nil || fn == nil {
		return ctx
	}
	return context.WithValue(ctx, agentProgressKey{}, fn)
}

func agentProgressFromCtx(ctx context.Context) func([]string) {
	if v, ok := ctx.Value(agentProgressKey{}).(func([]string)); ok {
		return v
	}
	return nil
}

// agentSteeringKey carries a MID-RUN steering source into a run (worker-only):
// a function the loop calls between steps to collect instructions a human gave
// while the agent was working. Before this, such a reply was dropped — the run
// was "already in flight" — so a correction ("use the other repo", "don't touch
// prod") only arrived after the agent had finished doing it the wrong way, and
// the only remedy was to stop the run and lose its progress.
//
// The runner stays storage-agnostic: it knows nothing about where the messages
// came from or how they are cleared, only that the source returns any new human
// turns. Every other caller leaves this unset, so their runs are unchanged.
type agentSteeringKey struct{}

// WithAgentSteering attaches a mid-run steering source to ctx. fn must be
// take-and-clear (each message returned exactly once) and cheap enough to call
// once per step; returning nil means "nothing new".
func WithAgentSteering(ctx context.Context, fn func() []string) context.Context {
	if ctx == nil || fn == nil {
		return ctx
	}
	return context.WithValue(ctx, agentSteeringKey{}, fn)
}

func agentSteeringFromCtx(ctx context.Context) func() []string {
	if v, ok := ctx.Value(agentSteeringKey{}).(func() []string); ok {
		return v
	}
	return nil
}

// agentStatusNoteKey carries a live status writer into a run (worker-only): a
// function that shows a short line on the run's own status surface (the evolving
// in-thread comment).
//
// It exists so a mid-run instruction can be ACKNOWLEDGED. Folding a human's note
// into the conversation silently is indistinguishable, from their side, from
// being ignored — which is exactly when people repeat themselves or stop the run.
// The runner stays surface-agnostic: it emits a sentence, the caller decides
// where it appears.
type agentStatusNoteKey struct{}

// WithAgentStatusNote attaches a status-line writer to ctx (worker-only).
func WithAgentStatusNote(ctx context.Context, fn func(string)) context.Context {
	if ctx == nil || fn == nil {
		return ctx
	}
	return context.WithValue(ctx, agentStatusNoteKey{}, fn)
}

func agentStatusNoteFromCtx(ctx context.Context) func(string) {
	if v, ok := ctx.Value(agentStatusNoteKey{}).(func(string)); ok {
		return v
	}
	return nil
}

// steeringAck is the line the agent shows when it picks up an instruction sent
// while it was working: a receipt, quoting the person back so they can see WHICH
// message landed (several may have arrived), and honest that it applies from now
// on rather than retroactively. Pure.
func steeringAck(msgs []string) string {
	kept := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m = strings.Join(strings.Fields(m), " "); m != "" {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	quote := kept[len(kept)-1]
	const max = 140
	if len([]rune(quote)) > max {
		quote = string([]rune(quote)[:max-1]) + "…"
	}
	if len(kept) > 1 {
		return "Got your notes — working them in from here. Latest: “" + quote + "”"
	}
	return "Got it — working that in from here: “" + quote + "”"
}

// steeringTurn renders human mid-run instructions as ONE user turn. It is
// explicit that these arrived after the work started and that they are
// authoritative, because the model otherwise treats a late instruction as
// background chatter and carries on with its original plan. Pure, so the wording
// is unit-testable and identical on every provider/protocol.
func steeringTurn(msgs []string) string {
	kept := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m = strings.TrimSpace(m); m != "" {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("A person sent this while you were working. It is the LATEST instruction and it overrides anything " +
		"earlier that conflicts with it — adjust what you are doing now, before your next action:\n")
	for _, m := range kept {
		sb.WriteString("\n- " + m)
	}
	sb.WriteString("\n\nIf it makes work you already did unnecessary, say so plainly rather than pretending it was asked for. " +
		"If it asks you to stop, wrap up and summarise what you completed.")
	return sb.String()
}

type toolCallRecord struct {
	Tool    string            `json:"tool"`
	Params  map[string]string `json:"params"`
	Result  string            `json:"result,omitempty"`
	Error   string            `json:"error,omitempty"`
	Skipped string            `json:"skipped,omitempty"`
	// Governance is a stable, machine-readable category for a call that was
	// gated by policy rather than by an ordinary failure, so the run transcript
	// can present a safety decision distinctly (not as a generic skip/error):
	//   "approval_required" — a write was routed to human approval (governed/
	//                         plan autonomy, or the destructive backstop);
	//   "blocked"           — the call was refused by policy (not on the
	//                         allow-list, or outside the agent's scope).
	// Empty for normal calls. Additive: unknown values degrade to no badge.
	Governance string `json:"governance,omitempty"`
	// Remote marks a call the remote brain answered itself, on its own
	// machine. This workspace did not run it and could not have refused it;
	// the record is the remote's account, kept so the transcript is whole.
	Remote bool `json:"remote,omitempty"`
}

// Governance category codes recorded on a gated tool call (see toolCallRecord).
const (
	govApprovalRequired = "approval_required"
	govBlocked          = "blocked"
)

type stepRecord struct {
	Iteration int              `json:"iteration"`
	Assistant string           `json:"assistant,omitempty"`
	ToolCalls []toolCallRecord `json:"tool_calls,omitempty"`
	// Steering holds the human instructions that arrived DURING the run and were
	// folded in before this step. Recorded so the transcript explains why the
	// agent changed course — without it, a mid-run correction looks like the model
	// randomly abandoning its plan.
	Steering []string `json:"steering,omitempty"`
	// Compaction, when set, records that the conversation was compacted right
	// before this step (older turns folded into a summary so the run could keep
	// going inside the model's context window). Purely informational — it keeps
	// a mechanism that is invisible at runtime visible in the audit trail.
	Compaction *compactionNote `json:"compaction,omitempty"`
}

// RunAgent executes the agent's tool-loop once. prompt is the run input (the
// manual test prompt, or a synthesized trigger context). It always records a
// run row and never panics out; errors (including a panicking tool executor)
// are captured in the outcome so a run row is never left stuck as "running".
// runIDPtr turns a run id into the nullable form the attribution column wants.
//
// uuid.Nil is not a run: CreateRun can fail, and RunAgent carries on so the
// agent still works without a transcript. Writing that zero uuid into a foreign
// key would fail the constraint and lose the whole proposal, so "no run" has to
// travel as NULL.
func runIDPtr(runID uuid.UUID) *uuid.UUID {
	if runID == uuid.Nil {
		return nil
	}
	return &runID
}

func RunAgent(ctx context.Context, agent *model.AiAgent, triggerSource, prompt string, dryRun bool) (out *RunOutcome) {
	// Taken before the run row, not after: CreateRun does a round trip, and a
	// span that starts when the row lands understates how long the agent took.
	startedAt := time.Now()
	// Who started this, for every audit row the run writes. Set here once so
	// the run summary, the drill and anything a tool records all agree; a
	// dispatcher that knows more (a delegation hop) has already set it.
	ctx = withRunInitiator(ctx, triggerSource)
	runID, _ := model.CreateRun(ctx, agent.Id, triggerSource, &agent.CreatedBy, prompt)
	out = &RunOutcome{RunID: runID, Status: model.RunRunning}
	steps := make([]stepRecord, 0, 4)

	// proposed accumulates the human-readable descriptions of writes an
	// approval-mode run queues for approval (instead of executing), so the
	// caller can disclose them deterministically.
	var proposed []string
	// toolsUsed accumulates the distinct tool names the run invoked (call
	// order), for the evaluation harness. toolNameSeen de-dupes by name.
	var toolsUsed []string
	toolNameSeen := map[string]bool{}
	// toolsSucceeded / failedTools partition the executed tools by outcome so a
	// user-facing disclosure reflects what genuinely happened (not what the
	// model narrated). De-duped by name.
	var toolsSucceeded []string
	var failedTools []string
	toolOkSeen := map[string]bool{}
	toolErrSeen := map[string]bool{}
	// plannedSteps accumulates the writes a "plan" autonomy run intends, to be
	// proposed as a SINGLE approval at the end (plan-approve).
	var plannedSteps []aiBusiness.PlanStep

	// Token/cost accounting for this run. runTokens is the metered, persisted
	// total (gates the per-run cap and the workspace daily budget); runCostUSD
	// is a best-effort estimate logged for audit only.
	var runTokens int64
	var runCostUSD float64

	finalize := func(status, result, errMsg string) *RunOutcome {
		out.Status = status
		out.Result = result
		out.Error = errMsg
		out.Steps = len(steps)
		out.Proposed = proposed
		out.ToolsUsed = toolsUsed
		out.ToolsSucceeded = toolsSucceeded
		out.FailedTools = failedTools
		// The run record is the ONLY trace of what the agent did, so it must land
		// even when the run was cut short: a cancelled run (a human stopped it, or
		// this worker lost its lease) has a cancelled ctx, and writing the row
		// through it left the run stuck as "running" forever. Detach.
		recCtx := context.WithoutCancel(ctx)
		// Plan-approve: if this "plan" run gathered writes, propose the whole
		// ordered plan as ONE durable approval (executes step-by-step as the
		// owner on approval). Best-effort; recorded in Proposed for disclosure.
		// Skipped when the run was cancelled: nobody wants an approval request
		// for work they just stopped.
		if len(plannedSteps) > 0 && ctx.Err() == nil {
			summary := aiBusiness.BuildPlanSummary(agent.Name, plannedSteps)
			if _, perr := aiBusiness.CreatePlanAction(ctx, agent.CreatedBy, "agent", agent.Id.String(), plannedSteps, summary, pendingModels.Attribution{AgentID: &agent.Id, RunID: runIDPtr(runID)}); perr != nil {
				helpers.LogErrorWithContext(ctx, "agentRunner: create plan action failed (agent=%s): %v", agent.Id, perr)
			} else {
				for _, s := range plannedSteps {
					out.Proposed = append(out.Proposed, planStepLabel(s))
				}
			}
		}
		stepsJSON, _ := json.Marshal(steps)
		finished := finishedRun{
			Agent:          agent,
			RunID:          runID,
			TriggerSource:  triggerSource,
			Status:         status,
			Error:          errMsg,
			ToolsSucceeded: toolsSucceeded,
			FailedTools:    failedTools,
			// Read off the transcript rather than accumulated separately: the
			// steps are the record of what policy actually decided, and a second
			// counter maintained by hand is a second thing to keep in step.
			GovernanceBlocks: governanceBlocksIn(steps),
			StartedAt:        startedAt,
			EndedAt:          time.Now(),
			DryRun:           dryRun,
		}
		if runID != uuid.Nil {
			_ = model.FinishRun(recCtx, runID, agent.Id, status, string(stepsJSON), len(steps), runTokens, result, errMsg)
			recordRunInAuditLog(recCtx, finished)
		}
		// Outside the guard, deliberately. A run whose row could not be written
		// is exactly the run OneCamp's own screens cannot show, so it is the one
		// the operator most needs to reach them somewhere else.
		emitRunSpan(recCtx, finished)
		if runTokens > 0 {
			helpers.LogInfoWithContext(recCtx, "agentRunner: run %s used %d tokens (~$%.4f), status=%s", runID, runTokens, runCostUSD, status)
		}
		return out
	}

	// A panicking tool executor must never leave the run "running" or 500 the
	// caller: recover, record the run as failed, and return a clean outcome.
	defer func() {
		if rec := recover(); rec != nil {
			helpers.LogErrorWithContext(ctx, "agentRunner: recovered panic (agent=%s): %v", agent.Id, rec)
			out = finalize(model.RunFailed, out.Result, "the agent hit an internal error")
		}
	}()

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return finalize(model.RunFailed, "", "AI is not enabled for this workspace")
	}

	userUUID := agent.CreatedBy.String()

	// Tools the agent is allowed to use.
	enabledTools := agentEnabledTools(agent)
	allow := make(map[string]bool, len(enabledTools))
	for _, t := range enabledTools {
		allow[t] = true
	}
	// Governed coding path: when code PRs are enabled and this agent can write
	// files to GitHub via a raw MCP tool, ensure it can ALSO reach code_pr — the
	// safe path that clones, edits, verifies and opens the PR (no hand-rolled
	// commit / stale-sha stall). This makes the redirect below actionable even
	// if the owner granted the raw GitHub tools but not code_pr.
	if ai.CodePREnabled() && !allow[codePRToolName] && hasGitHubContentWriteTool(allow) {
		allow[codePRToolName] = true
		enabledTools = append(enabledTools, codePRToolName)
	}

	// Optional scope: further restricts WHERE the agent may act (channels /
	// projects), on top of the owner's permissions. Empty = the owner's full
	// accessible scope.
	scopeCfg := agent.ScopeConfig()
	scopeChannels := toSet(scopeCfg.ChannelIDs)
	scopeProjects := toSet(scopeCfg.ProjectIDs)

	maxSteps := agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 1
	}
	if maxSteps > model.MaxStepsCeiling {
		maxSteps = model.MaxStepsCeiling
	}

	// A remote brain. The agent's reasoning happens at its AG-UI endpoint,
	// so there is no model to choose here: the remote is the provider, and
	// every model preference below is beside the point. Everything after the
	// provider (tools, rules, record) is the same for it as for any model.
	remote := agent.Remote()
	llm, cb, runLimits := svc.ResolveUserModelWithLimits(ctx, userUUID)
	if llm == nil && !remote {
		return finalize(model.RunFailed, "", "AI is not enabled for this workspace")
	}
	// Per-agent model (multi-model teammates): when the agent pins its own
	// authorized model, run on THAT provider+model with its own circuit breaker,
	// instead of the owner's personal preference. ResolveExplicitModel degrades
	// safely (default model) on any problem and honors local-only residency.
	//
	// runLimits tracks the winning model at every step of the precedence below, so the
	// run's context budget and compaction thresholds are the ones belonging to the
	// model that will actually read the prompt. A long agent loop is where this matters
	// most: it is the workload that fills a window, and it used to fill it to the
	// workspace default's size no matter which model an admin had chosen for the agent.
	agentModelLabel := ""
	if agent.ModelPref != nil {
		if mid, perr := uuid.Parse(strings.TrimSpace(*agent.ModelPref)); perr == nil {
			if am, merr := aiModels.GetAuthorizedModel(ctx, mid); merr == nil && am.Usable() {
				l, c, usedDefault, lim := svc.ResolveExplicitModelWithLimits(ctx, am.ProviderID, am.Model)
				llm, cb, runLimits = l, c, lim
				if !usedDefault {
					agentModelLabel = am.Model
				}
			}
		}
	}
	// Per-channel default model (Claude-Tag-style): when the agent has NOT
	// pinned its own model and this run has a channel scope, honor the model an
	// admin pinned for that channel. Precedence is agent > channel > workspace
	// default, and this branch is inert unless a channel model is set — so
	// existing runs are unaffected. Degrades safely to the default on any lookup
	// problem and respects local-only residency (via ResolveExplicitModel).
	// Delegates to the ONE resolver, which summarise and the channel @mention answer now
	// share. This used to be the same lookup chain written out inline here, and it was
	// the only place that honoured the pin — so the setting applied to agent runs and
	// silently not to any other AI work in the same channel.
	if agentModelLabel == "" {
		if sc := agentRunScopeFromCtx(ctx); sc.ChannelID != "" {
			if l, c, label, lim := aiBusiness.ChannelScopedLLM(ctx, sc.ChannelID); label != "" {
				llm, cb, agentModelLabel, runLimits = l, c, label, lim
			}
		}
	}

	if remote {
		p, perr := remoteProvider(agent, runID.String())
		if perr != nil {
			return finalize(model.RunFailed, "", perr.Error())
		}
		llm, cb, runLimits = p, p.Breaker(), ai.WorkspaceLimits()
		agentModelLabel = p.Label()
	}

	// Carry the winning model's limits for the rest of the run. Budget code downstream
	// (context assembly, compaction) reads them from the context rather than taking a
	// new parameter, matching how this package already carries per-run AI dimensions
	// such as the actor and the token budget.
	ctx = ai.WithModelLimits(ctx, runLimits)

	// Tag the context so any writes the agent performs do not re-trigger
	// workflows or other agents (cascade guard).
	ctx = helpers.WithWorkflowGenerated(ctx)

	// loopCtx carries the token-usage sink (providers report each call's usage
	// into it) and a wall-clock deadline so a slow or stuck run can't hang a
	// worker indefinitely. The per-run token cap and workspace daily budget
	// gate on the metered totals below.
	loopCtx, cancel := context.WithTimeout(ai.WithUsageSink(ctx), agentRunTimeout())
	defer cancel()
	// Attribute this run's AI spend to the AGENT'S OWN per-agent daily budget
	// (and enforce its cap) rather than the owner's personal seat quota: a busy
	// teammate must not eat the owner's interactive budget, and each teammate is
	// bounded independently. Matches how Claude Tag bills teammate work to the
	// org, not individual seats. The workspace cap still applies on top; a 0 cap
	// means "only the workspace cap" (the agent is still metered for reporting).
	loopCtx = ai.WithAgentBudget(loopCtx, agent.Id.String(), agent.MaxDailyTokens)
	// Thread the sandbox run identity + per-agent caps so the run_analysis tool
	// enforces the per-agent/per-channel budget tiers and attributes each run's
	// audit row to this agent/channel/run (the executor lives in another package
	// and has no other way to know them). Zero-value fields simply skip a tier.
	loopCtx = ai.WithSandboxScope(loopCtx, ai.SandboxScope{
		AgentID:           agent.Id.String(),
		ChannelID:         agentRunScopeFromCtx(ctx).ChannelID,
		RunID:             runID.String(),
		AgentDailySeconds: agent.SandboxDailySeconds,
		AgentDailyRuns:    agent.SandboxDailyRuns,
	})
	maxRunTokens := agentMaxRunTokens()
	modelLabel := ""
	if svc.Config != nil {
		modelLabel = svc.Config.ActiveModel()
	}
	// Prefer the agent's own pinned model label for run logging/attribution.
	if agentModelLabel != "" {
		modelLabel = agentModelLabel
	}

	// meterUsage folds whatever the provider reported for the LAST call into
	// this run's token + cost totals. Every model call in this function (loop
	// step, forced summary, self-critique, compaction summary) goes through it,
	// so no call can be silently unmetered and the cost estimate can't drift
	// from the token count.
	meterUsage := func() {
		if u := ai.TakeUsage(loopCtx); u.Total() > 0 {
			runTokens += int64(u.Total())
			runCostUSD += ai.CostUSD(modelLabel, u.InputTokens, u.OutputTokens)
		}
	}

	system := strings.TrimSpace(agent.Instructions)
	if system == "" {
		system = "You are an autonomous assistant operating inside the user's workspace. Complete the task you are given concisely."
	}
	// Temporal grounding: an LLM has no clock. Without the current date it
	// guesses (it once answered "commits since March 15, 2024" for "today").
	// Inject "now" so relative times ("today", "this week") resolve correctly.
	system += buildTemporalContext(time.Now())
	// The data boundary. On the text path it rides on the tool-results turn; on
	// the native path results arrive as tool-role messages this package never
	// composes, so it has to be said in the system prompt or not at all.
	system += ai.UntrustedContentRule
	system += replyStyleRule
	// Native function calling: prefer the provider's structured tools API when
	// it is available and enabled (reliable, no `<tool_call>` text parsing, and
	// the model cannot fabricate a result it wasn't given). When active we do
	// NOT inject the text tool prompt — tools are advertised via the API — which
	// also trims the system prompt. Falls back to the text protocol otherwise.
	toolCaller, _ := llm.(ai.ToolCallingProvider)
	// A remote agent always takes the native path, with however many tools it
	// has, including none: AG-UI carries tool calls as structure, and the
	// text protocol would ask a remote to imitate a format it has no reason
	// to know.
	useNative := toolCaller != nil && toolCaller.SupportsToolCalling() &&
		(remote || (agentNativeToolsEnabled() && len(enabledTools) > 0))
	var toolSpecs []ai.ToolSpec
	if useNative {
		toolSpecs = ai.ToolSpecsForRun(enabledTools, prompt)
		if len(toolSpecs) == 0 && !remote {
			useNative = false
		} else {
			// Advertise the registry-free control tools (needs_human,
			// save_progress, and — when the run has a channel/DM scope —
			// remember/forget) natively so the agent keeps those abilities
			// without the text directives.
			toolSpecs = append(toolSpecs, nativeControlToolSpecs(agentRunScopeFromCtx(ctx).hasScope())...)
		}
	}
	if !useNative {
		system += ai.BuildAgentToolPromptForRun(enabledTools, prompt)
	}
	// Resolve-before-ask/guess: an action often needs a concrete identifier the
	// human referred to only by name (a repository "owner/name", a project /
	// channel / user id, a file path, a ticket key). The correct move is to
	// DISCOVER it with the agent's own read tools (search / list / lookup) from
	// the name or context, then act — not to invent it and not to bounce the
	// question back to the human. This is provider-agnostic on purpose: it holds
	// for any connector or MCP server, so an agent behaves like a resourceful
	// teammate that figures out identifiers before asking. Only fall back to
	// needs_human when NO available tool can resolve it.
	system += "\n\nWhen an action needs an identifier you don't have yet (e.g. a repository owner/name, a " +
		"project/channel/user id, a file path, an issue/PR number), FIRST use your available search or list " +
		"tools to look it up from the name or context, then proceed. Only ask a human (needs_human) if no tool " +
		"can resolve it. Never invent an identifier, and never ask a human for something your tools can discover."
	// Generic blocker (needs_human): every agent may pause for a human when it
	// genuinely cannot proceed without a decision/input, rather than guessing or
	// failing. This is registry-free (intercepted in the loop below) so it is
	// always available regardless of the agent's tool allow-list.
	// Enumerating the answers is the difference between a question a person can
	// tap and one they have to compose a reply to. It also removes a guess: a
	// chosen option comes back as a fact, where free text has to be interpreted.
	const askWithOptions = "When the answer is one of a few known possibilities, list them in \"options\" " +
		"(2 to 6 short labels) instead of writing them into the question, and never ask for a password, key, " +
		"token or other credential."
	if useNative {
		system += "\n\nIf you are blocked and cannot proceed without a human decision or missing information, " +
			"do NOT guess or invent an answer — call the needs_human tool with a clear question and stop. " +
			askWithOptions + " A human will reply and you will resume from there."
	} else {
		system += "\n\nIf you are blocked and cannot proceed without a human decision or missing information, " +
			"do NOT guess or invent an answer. Instead emit a single tool call " +
			"<tool_call>{\"tool_name\":\"needs_human\",\"params\":{\"reason\":\"<what you need from a person, phrased as a clear question>\",\"options\":[\"<choice>\",\"<choice>\"]}}</tool_call> " +
			"and stop. " + askWithOptions + " A human will reply and you will resume from there."
	}
	// Truthfulness guard: weak models tend to narrate a confident success even
	// after a tool call returned an error. Tool results are fed back each step
	// (an error line begins with "error:"). Forbid claiming completed work that
	// a tool reported as failed, so the final summary matches reality.
	system += "\n\nNever claim you completed an action that a tool reported as failed. A tool result beginning " +
		"with \"error:\" means that action did NOT happen. When a tool errors, either retry it with corrected " +
		"input, call needs_human if you cannot proceed, or state plainly in your summary that it did not succeed " +
		"and why. Only report work as done when the tool result confirms it."
	// Single-pass guard: this run is the ONLY chance to answer — there is no
	// background continuation after it ends. Do NOT promise to follow up later
	// ("please wait", "I'll get back to you", "I'll search and reply"): finish
	// the work within this run and give the answer now, or (if truly blocked)
	// use needs_human. A stalling promise leaves the human waiting for a reply
	// that never comes.
	system += "\n\nYou run in a SINGLE pass: do the work now and give your answer in this run. Do NOT reply " +
		"with a promise to continue later (no \"please wait\", \"I'll get back to you\", \"I'll search and reply\"). " +
		"If you need a tool, call it now and then answer; if you cannot proceed, use needs_human."
	// Answer-the-latest-message guard: the run's input may include earlier
	// conversation for context. A weak model tends to blend an OLD parameter
	// (e.g. an earlier "past 3 days") with the CURRENT request (e.g. "today").
	// The most recent user message is authoritative: honor its scope/timeframe
	// exactly and ignore different scopes mentioned earlier.
	system += "\n\nAlways answer the user's MOST RECENT message. If it specifies a timeframe or scope " +
		"(e.g. \"today\", \"this week\", a specific repo), use exactly that and IGNORE any different timeframe " +
		"or scope mentioned earlier in the conversation — earlier messages are context only."
	// Continue-the-work: inject the agent's durable working notes from prior
	// runs (so a scheduled/long task advances slice-by-slice instead of starting
	// cold) and tell it how to update them. Best-effort; absent on a first run.
	if st, serr := model.GetAgentState(ctx, agent.Id); serr == nil && strings.TrimSpace(st) != "" {
		system += "\n\nYour working notes from previous runs (continue from where you left off; do not repeat finished work):\n\"\"\"\n" +
			strings.TrimSpace(st) + "\n\"\"\""
	}
	if !useNative {
		system += "\n\nYou may be re-run later. To carry progress forward across runs, emit " +
			"<tool_call>{\"tool_name\":\"save_progress\",\"params\":{\"notes\":\"<a concise, self-contained summary of what is done and what remains>\"}}</tool_call> " +
			"before finishing. It saves silently and you continue; keep the notes short and overwrite them each time."
	}

	// Conversational memory: inject the standing instructions people asked this
	// agent to remember for THIS channel/DM, and (text path) advertise the
	// remember/forget tools. Only when the run has a conversation scope, so a
	// manual/scheduled run spends no tokens on it.
	if runScope := agentRunScopeFromCtx(ctx); runScope.hasScope() {
		if mem := aiBusiness.AgentScopedMemoryBlock(ctx, runScope.ChannelID, runScope.GroupID); mem != "" {
			system += mem
		}
		if !useNative {
			system += "\n\nIf a person tells you to remember a standing instruction for this conversation, emit " +
				"<tool_call>{\"tool_name\":\"remember\",\"params\":{\"content\":\"<the instruction>\"}}</tool_call>; " +
				"to drop one, emit <tool_call>{\"tool_name\":\"forget\",\"params\":{\"query\":\"<text to match, or empty for all>\"}}</tool_call>. " +
				"Only use these when explicitly asked to remember/forget something."
			system += "\n\nIf a person asks you to do something on a schedule (a routine), emit " +
				"<tool_call>{\"tool_name\":\"create_routine\",\"params\":{\"prompt\":\"<what to do each run>\",\"recurrence\":\"FREQ=DAILY\",\"time\":\"09:00\",\"tz_offset_minutes\":\"0\"}}</tool_call> " +
				"(recurrence is FREQ=DAILY, FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR, or FREQ=HOURLY;INTERVAL=N for every-N-hours monitoring; include tz_offset_minutes so a local time is interpreted correctly). " +
				"To review or stop routines here, emit <tool_call>{\"tool_name\":\"list_routines\",\"params\":{}}</tool_call> or " +
				"<tool_call>{\"tool_name\":\"cancel_routine\",\"params\":{\"routine_id\":\"<id>\"}}</tool_call>. " +
				"Only use these when explicitly asked to schedule, list, or cancel recurring work."
		}
	}

	// Per-agent knowledge sources: prepend grounding read AS THE OWNER from the
	// agent's curated channels/docs/projects (permission-checked, size-bounded).
	kc := buildKnowledgeContext(ctx, agent)
	system += kc
	// Charts. Every surface a run's words can land on draws a ```chart block:
	// a channel post, a DM, a comment, a document, the run's own result in the
	// builder. Until the runner said so, the model did not know, and an agent
	// asked for a weekly report wrote the numbers out in a sentence.
	system += chartCapabilityFor(enabledTools, kc)
	// GitHub grounding: give a tool-enabled agent the workspace's connected
	// repos (exact owner/name) so it resolves "the onecamp-fe repo" to
	// akashc777/onecamp-fe deterministically instead of a global search or a
	// "which repository?" question.
	if len(enabledTools) > 0 {
		if gh := buildGitHubContext(ctx); gh != "" {
			system += gh
		}
	}
	// Reusable skills: compose the agent's referenced skill modules into the
	// system prompt (edits to a skill take effect on the next run). The
	// fingerprints travel with the run so a later edit cannot change what this
	// run appears to have been asked.
	sk, skillsUsed := buildSkillsPrompt(ctx, agent)
	if sk != "" {
		system += sk
	}
	// Data visualization: a data-capable agent may render an inline chart
	// (```chart JSON). Every surface an agent posts to now draws it — the
	// assistant panel and channel/thread/DM/group messages (the BotPost stream
	// converts the block into a chart embed) — so this is worth advertising.
	// Gated on data-bearing tools so a conversational agent spends no budget on it.
	system += buildChartCapabilityPrompt(enabledTools)

	userMsg := strings.TrimSpace(prompt)
	if userMsg == "" {
		userMsg = "Run now. Use your tools as needed to accomplish your purpose, then summarize what you did."
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: userMsg},
	}
	opts := ai.ChatOptions{Temperature: 0.3, MaxTokens: 2048}

	// What this agent was told, recorded now rather than at the end.
	//
	// The prompt is fully composed at this point and the model precedence has
	// been resolved, so this is the first and only moment both are known. Doing
	// it here also means a run that crashes or times out still carries the
	// instructions that produced it, which is the run somebody will need to
	// explain.
	//
	// Fingerprint, not a copy: it proves what was sent without storing the
	// prompt twice, and because a fingerprint is not content it survives the
	// retention sweep that clears the transcript.
	if runID != uuid.Nil {
		runModel := agentModelLabel
		if runModel == "" {
			// No pin, so the workspace default won. Record which model that was,
			// because the default is a setting and settings change.
			runModel = svc.ChatModelLabel()
		}
		if perr := model.RecordRunProvenance(ctx, runID, runModel,
			helpers.SHA256Hex(system), skillsUsedJSON(skillsUsed)); perr != nil {
			helpers.LogErrorWithContext(ctx, "agentRunner: could not record provenance for run %s: %+v", runID, perr)
		}
	}

	seen := make(map[string]bool) // dedupe identical tool calls across the run
	// priorResults keeps what each call returned, by signature. A model that
	// repeats a call has usually lost its result to compaction (on the demo the
	// same search ran into "duplicate call skipped" four times after a fold), so
	// a repeat is answered with the earlier result instead of a bare refusal.
	priorResults := make(map[string]string)

	// Durable resume (worker-only): when a saved conversation is supplied,
	// continue it instead of starting cold, and rebuild the dedupe set from the
	// prior assistant tool calls so a write that already ran is never repeated
	// on resume. Other callers never set this, so their behavior is unchanged.
	resume := agentResumeFromCtx(ctx)
	if resume != nil && len(resume.messages) > 0 {
		messages = resume.messages
		for _, m := range messages {
			if m.Role != "assistant" {
				continue
			}
			// Text path: parse <tool_call> blocks out of the content.
			if _, acts := ai.ParseToolCalls(m.Content); len(acts) > 0 {
				for _, a := range acts {
					seen[ai.ActionSignature(a)] = true
				}
			}
			// Native path: the prior assistant turn carries structured tool
			// calls; seed the dedupe set from those too so a resumed native run
			// never re-runs a write it already performed.
			for _, c := range m.ToolCalls {
				seen[ai.ActionSignature(ai.ToolCallToAction(c))] = true
			}
		}
	}
	checkpoint := func() {
		if resume != nil && resume.checkpoint != nil {
			resume.checkpoint(messages)
		}
	}
	// Optional per-step progress (worker-only): report the tools used so far so
	// a live status surface can show "working… (used: …)". No-op otherwise.
	progress := agentProgressFromCtx(ctx)
	reportProgress := func() {
		if progress != nil {
			progress(append([]string(nil), toolsUsed...))
		}
	}
	// Optional mid-run steering (worker-only): instructions a human gave while
	// the agent was working. Drained BETWEEN steps — never mid-turn — so a tool
	// result is never separated from the call it answers. pendingSteering is
	// attached to the next step's transcript record so the change of course is
	// explained rather than mysterious.
	steer := agentSteeringFromCtx(ctx)
	var pendingSteering []string
	// Human-authored turns of this run, for the memory guard. The prompt is what
	// a person (or a trigger acting for one) asked for; steering is what they
	// said while it worked. Tool results are deliberately absent: that is the
	// whole distinction the guard rests on.
	humanTurns := []string{prompt}
	loopCtx = WithAgentHumanText(loopCtx, func() []string {
		return append([]string(nil), humanTurns...)
	})

	finalText := ""
	// stallRetries bounds the plan/placeholder corrective nudges within this run
	// (see agentStall.go): a weak model that narrates a plan or fabricates a
	// placeholder answer without calling any tool gets pushed to actually act.
	stallRetries := 0
	deflectionRetries := 0
	// claimRetries bounds the deterministic honesty corrections within this run
	// (see agentVerifyClaims.go): a draft that claims success while a WRITE it
	// ran actually failed gets pushed once to retry or rewrite honestly.
	claimRetries := 0
	// Rate-limit fallback: a SECONDARY local model (no per-day cap) to switch to
	// if the primary is quota-exhausted (HTTP 429) on the FIRST call, so the run
	// still completes instead of hard-failing. Resolved once; nil when no
	// distinct local model is configured.
	var fallbackLLM ai.LLMProvider
	var fallbackCB *ai.CircuitBreaker
	if !remote {
		// A remote brain has no local stand-in: switching to a workspace model
		// mid-run would quietly turn somebody else's agent into ours.
		fallbackLLM, fallbackCB = svc.ResolveFallbackModel(loopCtx)
	}
	switchedToFallback := false

	// Context compaction state. A tool-loop conversation only grows (assistant
	// turn + one observation per call, every step), so a long run used to walk
	// into the model's context window: a local model silently loses the system
	// prompt from the front, a cloud model returns a hard 400 and the run fails
	// having thrown away every completed step. Instead, when the conversation
	// approaches the usable input budget we fold the OLDER turns into a summary
	// block (plus mechanically extracted working state and the human's own
	// messages, verbatim) and keep going. compaction carries the notes forward
	// across rounds; pendingCompaction is attached to the next step's transcript
	// record so the fold is auditable.
	var compaction *ai.CompactionState
	var pendingCompaction *compactionNote
	compactions, contextRescues := 0, 0
	// compactNow folds the conversation in place and reports whether it did.
	// Bounded per run: a conversation that still doesn't fit after
	// maxRunCompactions folds is left to the ordinary step/token bounds, which
	// finalize with the partial result instead of looping on summaries.
	compactNow := func(rescue bool) bool {
		if compactions >= maxRunCompactions {
			return false
		}
		res, note := compactRunConversation(loopCtx, llm, messages, compaction, opts.MaxTokens, modelLabel, agent.Id, rescue)
		// The summarizer is a model call: meter it like every other one.
		meterUsage()
		if !res.Compacted {
			return false
		}
		compactions++
		messages = res.Messages
		compaction = res.State
		pendingCompaction = note
		// Persist the compacted conversation so a pause/crash resumes from the
		// smaller view rather than re-loading the oversized one. The full
		// transcript stays in the run record (steps), so nothing auditable is
		// lost by shrinking what we send.
		checkpoint()
		return true
	}

	// finalizeStopped records a stable stop-reason code alongside the clean
	// RunStopped outcome, so a durable caller classifies the stop on the code
	// rather than the message text.
	finalizeStopped := func(reason, result, errMsg string) *RunOutcome {
		out.StopReason = reason
		return finalize(model.RunStopped, result, errMsg)
	}

	// forceSummarize turns completed tool work into a plain-text answer when a
	// run hit a hard bound (step / per-run token limit) with the last turn being
	// a tool call, so it left no final summary. Without this, a run that DID the
	// work returns an empty result and the surface shows an unhelpful "mention me
	// again" — and this hurts stronger models most, since they take more tool
	// steps and are likelier to hit the bound mid-loop. It is fully model
	// agnostic: it relies only on a plain chat turn (no tool-call format), so it
	// behaves identically across every provider/model. Best-effort: on any
	// breaker/budget/error it returns the existing text unchanged.
	forceSummarize := func(existing string) string {
		if strings.TrimSpace(existing) != "" || len(toolsUsed) == 0 {
			return existing
		}
		if err := cb.Allow(); err != nil {
			return existing
		}
		if exceeded, _ := ai.TokenBudgetExceeded(loopCtx); exceeded {
			return existing
		}
		sumMsgs := append(append([]ai.ChatMessage{}, messages...), ai.ChatMessage{
			Role:    "user",
			Content: "Stop here and do not call any more tools. In plain text, give a concise summary of what you found and did based on the tool results above. If the tools did not return what was needed, say so plainly.",
		})
		// Rescued like any other one-shot call: this is a SEPARATE request from the
		// loop's, so the loop's own compaction rescue does not cover it, and it carries
		// the whole tool transcript — the largest prompt the run ever builds.
		ans, err := ai.ChatWithRescue(loopCtx, llm, sumMsgs, ai.ChatOptions{Temperature: 0.3, MaxTokens: 1024})
		if err != nil {
			cb.RecordResult(err)
			return existing
		}
		cb.RecordSuccess()
		meterUsage()
		clean, _ := ai.ParseToolCalls(ai.StripReasoning(ans))
		if clean = strings.TrimSpace(clean); clean != "" {
			return clean
		}
		return existing
	}

	// verifyAnswer runs an OPTIONAL self-critique pass (evaluator-optimizer): it
	// shows the model its own draft answer alongside the tool results already in
	// the conversation and asks it to keep the answer as-is when every claim is
	// supported, or correct it when not. Catches a confident-but-unsupported
	// final answer the heuristic guards miss. OFF by default (one extra model
	// call); enable with AI_AGENT_VERIFY=true, ideally on a capable model. Runs
	// only when a tool actually SUCCEEDED (there is something to verify against)
	// and the breaker/budget allow. Best-effort: any problem returns the draft
	// unchanged, so verification can never turn a good answer into a failure.
	verifyAnswer := func(draft string) string {
		if !agentVerifyEnabled() || strings.TrimSpace(draft) == "" || len(toolsSucceeded) == 0 {
			return draft
		}
		if err := cb.Allow(); err != nil {
			return draft
		}
		if exceeded, _ := ai.TokenBudgetExceeded(loopCtx); exceeded {
			return draft
		}
		vMsgs := append(append([]ai.ChatMessage{}, messages...), ai.ChatMessage{
			Role:    "user",
			Content: buildVerifyPrompt(draft),
		})
		ans, err := ai.ChatWithRescue(loopCtx, llm, vMsgs, ai.ChatOptions{Temperature: 0.2, MaxTokens: 1024})
		if err != nil {
			cb.RecordResult(err)
			return draft
		}
		cb.RecordSuccess()
		meterUsage()
		clean, _ := ai.ParseToolCalls(ai.StripReasoning(ans))
		if clean = strings.TrimSpace(clean); clean != "" {
			return clean
		}
		return draft
	}

	for i := 0; i < maxSteps; i++ {
		if err := cb.Allow(); err != nil {
			return finalizeStopped(StopReasonCircuitOpen, finalText, "AI temporarily unavailable (circuit open)")
		}
		if err := svc.Resiliency.CheckRateLimit(loopCtx, userUUID); err != nil {
			return finalizeStopped(StopReasonRateLimited, finalText, "rate limit reached")
		}
		if exceeded, scope := ai.TokenBudgetExceeded(loopCtx); exceeded {
			msg := "the workspace AI token budget for today has been reached"
			reason := StopReasonWorkspaceBudget
			switch scope {
			case ai.BudgetScopeUser:
				msg = "the daily AI token budget for this agent's owner has been reached"
				reason = StopReasonUserBudget
			case ai.BudgetScopeAgent:
				msg = "this agent's daily token budget has been reached"
				reason = StopReasonAgentBudget
			case ai.BudgetScopeChannel:
				msg = "this channel's daily AI token budget has been reached"
				reason = StopReasonChannelBudget
			}
			return finalizeStopped(reason, finalText, msg)
		}

		// Mid-run steering: fold in anything a human said since the last step, as
		// a normal user turn, so the model treats it as the latest instruction
		// instead of finishing the job the way it was originally asked. Checkpointed
		// immediately: a crash must not lose an instruction that was already
		// consumed from the inbox.
		if steer != nil {
			if fresh := steer(); len(fresh) > 0 {
				if turn := steeringTurn(fresh); turn != "" {
					messages = append(messages, ai.ChatMessage{Role: "user", Content: turn})
					pendingSteering = append(pendingSteering, fresh...)
					// Steering is a person talking, so it counts as human
					// provenance for the memory guard.
					humanTurns = append(humanTurns, fresh...)
					checkpoint()
					// Tell the person their note landed. Without this, steering is
					// invisible until the final answer — which reads as being
					// ignored, and is when people repeat themselves or stop the run.
					if note := agentStatusNoteFromCtx(ctx); note != nil {
						if ack := steeringAck(fresh); ack != "" {
							note(ack)
						}
					}
					helpers.LogInfoWithContext(ctx, "agentRunner: folded %d mid-run instruction(s) (agent=%s)", len(fresh), agent.Id)
				}
			}
		}

		// Proactive compaction: fold older turns BEFORE the prompt outgrows the
		// window, so the model never loses its system prompt to front-truncation
		// and the provider never rejects the request. No-op on a short
		// conversation, so a one-step run is byte-identical to before.
		// loopCtx carries this run's model limits, so the trigger is measured against
		// the window of the model the agent is actually running on.
		if ai.ShouldCompactConversation(loopCtx, messages, opts.MaxTokens) {
			compactNow(false)
		}

		var answer string
		var actions []ai.ProposedAction
		var nativeCalls []ai.ToolCall
		var err error
		if useNative {
			var calls []ai.ToolCall
			answer, calls, err = toolCaller.ChatWithTools(loopCtx, messages, toolSpecs, opts)
			nativeCalls = calls
			for _, c := range calls {
				actions = append(actions, ai.ToolCallToAction(c))
			}
		} else {
			// chat-rescue-exempt: the loop below runs a BETTER rescue for this call —
			// a summarising compaction that preserves what the run learned, carried
			// forward across steps. ChatWithRescue would shrink mechanically first
			// and this call would never see the overflow, silently downgrading the
			// agent's recovery to dropping its own history.
			answer, err = llm.Chat(loopCtx, messages, opts)
		}
		if err != nil {
			// Context overflow rescue: the provider refused the prompt as too
			// large (our estimate mispredicted — a different tokenizer, or a
			// window smaller than configured). That is OUR bookkeeping problem,
			// not a provider fault, so it must not count against the circuit
			// breaker: compact and retry the SAME step on the smaller
			// conversation. Bounded by maxContextRescues so a prompt that can't
			// be shrunk fails cleanly instead of looping.
			if ai.IsContextOverflow(err) && contextRescues < maxContextRescues {
				contextRescues++
				if compactNow(true) {
					i-- // a rescue must not cost the agent a step
					continue
				}
			}
			cb.RecordResult(err)
			// Rate-limit / quota exhaustion on the PRIMARY model: degrade to the
			// local fallback model (no per-day cap) once, on the first call
			// (before any tool has run, so the message thread is just
			// system+user and switching native->text is clean), then retry the
			// SAME step. This turns "the AI model call failed" on a spent Groq
			// quota into a completed run on the local backup.
			if !switchedToFallback && len(toolsUsed) == 0 && fallbackLLM != nil &&
				(errors.Is(err, ai.ErrProviderRateLimited) || errors.Is(err, ai.ErrRateLimited)) {
				switchedToFallback = true
				wasNative := useNative
				llm = fallbackLLM
				if fallbackCB != nil {
					cb = fallbackCB
				}
				toolCaller, _ = llm.(ai.ToolCallingProvider)
				useNative = agentNativeToolsEnabled() && toolCaller != nil && toolCaller.SupportsToolCalling() && len(toolSpecs) > 0
				// If the fallback can't do native tools but the primary did, the
				// system prompt has no tool instructions — append the text tool
				// prompt now so the fallback model can still call tools.
				if wasNative && !useNative && len(messages) > 0 && messages[0].Role == "system" {
					messages[0].Content += ai.BuildAgentToolPromptForRun(enabledTools, prompt)
				}
				helpers.LogInfoWithContext(ctx, "agentRunner: primary rate-limited, falling back to local model (agent=%s)", agent.Id)
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return finalizeStopped(StopReasonRunTimeout, finalText, "reached the run time limit")
			}
			// Cancelled, not broken: a human stopped the job, or this worker lost
			// its lease. Reporting "the AI model call failed" here was actively
			// misleading (and, for a lease loss, made the durable caller take the
			// failure path). Stop cleanly and keep whatever the run produced.
			if errors.Is(err, context.Canceled) {
				return finalizeStopped(StopReasonCanceled, finalText, "stopped before finishing")
			}
			if errors.Is(err, ai.ErrUserTokenBudgetExceeded) {
				return finalizeStopped(StopReasonUserBudget, finalText, "the daily AI token budget for this agent's owner has been reached")
			}
			if errors.Is(err, ai.ErrAgentTokenBudgetExceeded) {
				return finalizeStopped(StopReasonAgentBudget, finalText, "this agent's daily token budget has been reached")
			}
			if errors.Is(err, ai.ErrChannelTokenBudgetExceeded) {
				return finalizeStopped(StopReasonChannelBudget, finalText, "this channel's daily AI token budget has been reached")
			}
			if errors.Is(err, ai.ErrWorkspaceTokenBudgetExceeded) {
				return finalizeStopped(StopReasonWorkspaceBudget, finalText, "the workspace AI token budget for today has been reached")
			}
			helpers.LogErrorWithContext(ctx, "agentRunner: model call failed (agent=%s) err: %+v", agent.Id, err)
			return finalize(model.RunFailed, finalText, "the AI model call failed")
		}
		cb.RecordSuccess()

		// Meter this call's tokens into the run total and the cost estimate.
		// The workspace + per-user daily meters are incremented at the provider
		// layer (covers every AI caller), so we do not double-record here.
		meterUsage()

		// Strip any reasoning-model chain-of-thought (<think>…</think>) before
		// parsing/usage: otherwise a reasoning model's private deliberation gets
		// posted verbatim as the reply, and a turn that is ONLY reasoning is
		// mistaken for a final answer so the loop stops before it ever acts.
		answer = ai.StripReasoning(answer)

		cleanText := strings.TrimSpace(answer)
		if !useNative {
			cleanText, actions = ai.ParseToolCalls(answer)
			cleanText = strings.TrimSpace(cleanText)
		}
		step := stepRecord{Iteration: i + 1, Assistant: cleanText, Steering: pendingSteering, Compaction: pendingCompaction}
		pendingSteering, pendingCompaction = nil, nil
		// What the remote did on its own machine during this turn. Recorded
		// as its own account, never executed and never governed here, so the
		// transcript is complete about what happened without claiming a
		// control it did not have.
		step.ToolCalls = append(step.ToolCalls, remoteWorkRecords(llm)...)

		// A pause written as text instead of called: act on it as the call.
		if len(actions) == 0 {
			if a, ok := blockerCallFromText(cleanText); ok {
				actions = []ai.ProposedAction{a}
				cleanText = ""
				step.Assistant = ""
			}
		}

		if len(actions) == 0 {
			// Stall/placeholder guard: a weak model sometimes NARRATES a plan or
			// fabricates a templated answer ("I'll search… please wait… assuming
			// the tool results are available, here is: [list of commits]"), OR —
			// after a tool it called ERRORED — narrates "since the tool call is
			// not actually executed, I'll give a general response" and invents
			// one. Posting either is misleading. When the agent HAS tools but NONE
			// SUCCEEDED (nothing tried, or everything errored) and this tool-free
			// turn reads as a non-answer, nudge it once to actually act instead of
			// accepting the fabrication.
			if len(allow) > 0 && len(toolsSucceeded) == 0 && looksLikeNonAnswer(cleanText) {
				steps = append(steps, step)
				if stallRetries < maxStallRetries {
					stallRetries++
					messages = append(messages,
						ai.ChatMessage{Role: "assistant", Content: cleanText},
						ai.ChatMessage{Role: "user", Content: stallCorrection},
					)
					checkpoint()
					continue
				}
				// Still only a plan/placeholder after the nudge, with no tool
				// having succeeded: fail cleanly rather than post fabricated text.
				// The caller's honest "couldn't put together a reply" fallback is
				// shown instead of a fake result; a durable task re-grounds and
				// retries.
				out.StopReason = StopReasonStepLimit
				return finalize(model.RunFailed, "", "the model described a plan without using any tool")
			}
			// A short hand-back ("ready for the next step") is not an answer,
			// whatever tools ran before it. One nudge to do the work or report.
			// Delegated work (a durable job) that has not tried to change
			// anything may not end by asking which thing to do.
			attemptedChange := len(succeededWriteTools(toolsSucceeded)) > 0 || len(proposed) > 0 || len(plannedSteps) > 0 ||
				len(succeededWriteTools(failedTools)) > 0
			if handsBackWork(cleanText, agentResumeFromCtx(ctx) != nil, attemptedChange) && deflectionRetries < maxDeflectionRetries {
				deflectionRetries++
				steps = append(steps, step)
				messages = append(messages,
					ai.ChatMessage{Role: "assistant", Content: cleanText},
					ai.ChatMessage{Role: "user", Content: deflectionCorrection},
				)
				checkpoint()
				continue
			}
			// Deterministic honesty gate: never ship a "done" that a FAILED
			// write contradicts. If the draft asserts success while a write tool
			// it ran errored (and it doesn't already own the failure), push it
			// ONCE to retry the action or rewrite honestly — no extra model call,
			// driven purely by ground-truth execution facts. This is the
			// "verifier loop" that stops a confident-but-false completion.
			if verdict := verifyRunClaims(cleanText, failedWriteTools(failedTools)); verdict.NeedsCorrection && claimRetries < maxClaimRetries {
				claimRetries++
				steps = append(steps, step)
				messages = append(messages,
					ai.ChatMessage{Role: "assistant", Content: cleanText},
					ai.ChatMessage{Role: "user", Content: claimCorrection(verdict.FailedTools)},
				)
				checkpoint()
				continue
			}
			// The other half of the same question, and the half that was missing.
			//
			// The gate above only fires when a write was ATTEMPTED and errored.
			// An agent that reads a couple of things, never calls a write tool at
			// all, and then reports "I've updated the doc" passed every check:
			// nothing failed, so there was no failure to contradict; a read
			// succeeded, so the stall guard stayed quiet; and confident prose does
			// not look like a non-answer. The run was recorded as succeeded and
			// the work did not exist.
			//
			// Same shape, same cost, same ground truth: the run's execution
			// ledger, not the model's account of it.
			if verdict := verifyWorkHappened(cleanText, succeededWriteTools(toolsSucceeded)); verdict.NeedsCorrection && claimRetries < maxClaimRetries {
				claimRetries++
				steps = append(steps, step)
				messages = append(messages,
					ai.ChatMessage{Role: "assistant", Content: cleanText},
					ai.ChatMessage{Role: "user", Content: noWriteCorrection},
				)
				helpers.LogInfoWithContext(ctx,
					"agentRunner: run %s claimed a change with no write tool having succeeded; asked it to correct or perform the action", runID)
				checkpoint()
				continue
			}
			// No more actions: this turn is the final summary. Optionally run a
			// self-critique pass to catch an unsupported claim before posting.
			finalText = verifyAnswer(cleanText)
			steps = append(steps, step)
			return finalize(model.RunSucceeded, finalText, "")
		}

		// Lookups in one turn are independent of each other, so run them at the
		// same time instead of paying the sum of their round trips while a person
		// watches a "working…" comment. ONLY read-only calls that the sequential
		// path below would execute verbatim are eligible — every governance check
		// still happens exactly once, in that one place — and results are consumed
		// in the model's original call order.
		prefetched := prefetchReadOnlyTools(loopCtx, actions, userUUID, func(a ai.ProposedAction) bool {
			switch {
			case !ai.ToolIsReadOnly(a.ToolName): // writes stay strictly sequential
				return false
			case ai.ToolNeedsHumanBeforeUnattended(a.ToolName): // belt and braces: never pre-run one
				return false
			case isControlTool(a.ToolName): // self-state/blocker tools aren't registry calls
				return false
			case !allow[a.ToolName]:
				return false
			case ai.ValidateAction(a) != nil:
				return false
			case outsideScope(a, scopeChannels, scopeProjects):
				return false
			case seen[ai.ActionSignature(a)]: // already done earlier in this run
				return false
			}
			_, ok := ai.GetExecutor(a.ToolName)
			return ok
		})

		var resultLines []string
		var obsList []string // per-action observation, parallel to actions (native tool-result threading)
		// A terminal tool (e.g. code_pr) can end the turn with its own ack as the
		// final reply; tracked across this turn's actions and applied after all
		// of them have executed + been recorded.
		var terminalReply string
		terminalFired := false
		for _, a := range actions {
			rec := toolCallRecord{Tool: a.ToolName, Params: a.Params}
			repeatOf := "" // set when this call repeats one that already returned
			if !toolNameSeen[a.ToolName] {
				toolNameSeen[a.ToolName] = true
				toolsUsed = append(toolsUsed, a.ToolName)
			}
			// Generic blocker: the agent declared it needs a human decision.
			// Stop cleanly with the reason so a durable caller can park the job
			// as awaiting_input and resume it on a human reply. Registry-free:
			// handled here regardless of the allow-list.
			if a.ToolName == blockerToolName {
				elic := newElicitation(a.Params["reason"], a.Params["options"])
				// MCP makes this a MUST: elicitation may not be used to request
				// sensitive information. Refused as a tool ERROR rather than a
				// pause, so the loop continues and the model can correct itself
				// — pausing would put the request in front of a person, which is
				// the exact outcome the rule exists to prevent.
				// Same shape for an internal id: nobody can answer it, so the
				// model is told to look it up rather than put it to a person.
				if internalIDElicitation(elic.Question) {
					rec.Error = internalIDElicitationRefusal
					step.ToolCalls = append(step.ToolCalls, rec)
					helpers.LogInfoWithContext(ctx,
						"agentRunner: agent %s asked a person for an internal id; told to look it up", agent.Id)
					continue
				}
				if sensitiveElicitation(elic.Question) {
					rec.Error = sensitiveElicitationRefusal
					step.ToolCalls = append(step.ToolCalls, rec)
					helpers.LogInfoWithContext(ctx,
						"agentRunner: agent %s asked a person for a credential; refused", agent.Id)
					continue
				}
				rec.Result = "paused — waiting for a human"
				step.ToolCalls = append(step.ToolCalls, rec)
				steps = append(steps, step)
				out.Blocked = true
				out.BlockReason = elic.Question
				out.BlockOptions = elic.Options
				// Result carries the RENDERED question, so every surface that
				// shows a stopped run's result shows the options too without
				// each one re-deriving them.
				return finalizeStopped(StopReasonBlocked, elic.Render(), "")
			}
			// Continue-the-work: persist the agent's durable state blob and keep
			// going. Self-state (not an external write), so it always runs and is
			// never approval-gated. Registry-free.
			if a.ToolName == progressToolName {
				notes := strings.TrimSpace(a.Params["notes"])
				if serr := model.SetAgentState(ctx, agent.Id, notes); serr != nil {
					rec.Error = "could not save progress: " + serr.Error()
				} else {
					rec.Result = "progress saved"
				}
				step.ToolCalls = append(step.ToolCalls, rec)
				obs := rec.Result
				if rec.Error != "" {
					obs = "error: " + rec.Error
				}
				resultLines = append(resultLines, fmt.Sprintf("%s -> %s", a.ToolName, obs))
				obsList = append(obsList, obs)
				continue
			}
			// Conversational memory (remember/forget): a person can give the
			// agent a standing instruction for THIS channel/DM, or drop one.
			// Scope-bound + governed (workspace memory), so it is self-knowledge
			// like save_progress — always runs, never approval-gated. Registry-free.
			if a.ToolName == rememberToolName || a.ToolName == forgetToolName {
				handleMemoryTool(ctx, agent, a, &rec)
				step.ToolCalls = append(step.ToolCalls, rec)
				obs := rec.Result
				if rec.Error != "" {
					obs = "error: " + rec.Error
				} else if rec.Skipped != "" {
					obs = "skipped: " + rec.Skipped
				}
				resultLines = append(resultLines, fmt.Sprintf("%s -> %s", a.ToolName, obs))
				obsList = append(obsList, obs)
				continue
			}
			// Conversational routines (create/list/cancel): a person can hand the
			// agent standing recurring work for THIS channel/DM. Scope-bound
			// self-configuration (not an external write), so it always runs and is
			// never approval-gated. Registry-free.
			if isRoutineTool(a.ToolName) {
				handleRoutineTool(ctx, agent, a, &rec)
				step.ToolCalls = append(step.ToolCalls, rec)
				obs := rec.Result
				if rec.Error != "" {
					obs = "error: " + rec.Error
				} else if rec.Skipped != "" {
					obs = "skipped: " + rec.Skipped
				}
				resultLines = append(resultLines, fmt.Sprintf("%s -> %s", a.ToolName, obs))
				obsList = append(obsList, obs)
				continue
			}
			switch {
			case !allow[a.ToolName]:
				rec.Skipped = "tool not permitted for this agent"
				rec.Governance = govBlocked
			case ai.ValidateAction(a) != nil:
				rec.Skipped = ai.ValidateAction(a).Error()
			case outsideScope(a, scopeChannels, scopeProjects):
				rec.Skipped = "target is outside this agent's allowed scope"
				rec.Governance = govBlocked
			case ai.CodePREnabled() && allow[codePRToolName] && isGitHubContentWriteTool(a.ToolName):
				// Governed coding path: opening a PR / changing files by hand
				// via the raw GitHub Contents API forces the model to juggle a
				// per-file blob sha across turns and stalls on a stale sha. The
				// code_pr tool does this reliably (clone → edit → verify → PR).
				// Refuse the raw write and steer the model to code_pr — it self-
				// corrects, and no PR is ever left half-built. Reads/comments are
				// unaffected (only file-content writes match).
				rec.Skipped = "to change files or open a pull request, call the code_pr tool with a clear `instruction` — it makes the change in an isolated sandbox, verifies it, and opens the PR for review. Do NOT write files with raw GitHub tools (create_or_update_file / push_files); that path is disabled for agents while code PRs are enabled."
				rec.Governance = govBlocked
			default:
				sig := ai.ActionSignature(a)
				if seen[sig] {
					rec.Skipped = "duplicate call skipped"
					repeatOf = priorResults[sig]
					break
				}
				seen[sig] = true
				exec, ok := ai.GetExecutor(a.ToolName)
				if !ok {
					rec.Skipped = "this tool is currently unavailable — its connector or MCP server may be disconnected or disabled"
					break
				}
				if dryRun && !ai.ToolIsReadOnly(a.ToolName) {
					rec.Skipped = "dry-run: write not executed"
					break
				}
				// Plan-approve ("plan" mode): the agent does the read/think work
				// itself, but a WRITE is gathered into an ordered plan proposed
				// as ONE approval at the end (executed step-by-step as the owner
				// on approval). Read-only tools always run.
				if agent.Autonomy == model.AutonomyPlan && !ai.ToolIsReadOnly(a.ToolName) {
					plannedSteps = append(plannedSteps, aiBusiness.PlanStep{
						ToolName: a.ToolName, Params: a.Params, Description: actionLabel(a),
					})
					rec.Skipped = "added to plan — awaiting approval"
					rec.Governance = govApprovalRequired
					break
				}
				// Governed autonomy ("approval" mode): the agent does all the
				// read/think work itself, but a WRITE is proposed as a durable
				// pending action a human must approve — it then executes AS the
				// owner with permissions re-checked (reuses the in-thread
				// approval mechanism). Read-only tools always run.
				if agent.Autonomy == model.AutonomyApproval && !ai.ToolIsReadOnly(a.ToolName) {
					if _, perr := aiBusiness.CreatePendingAction(loopCtx, agent.CreatedBy, "agent", agent.Id.String(), a.ToolName, a.Params, proposalDescription(agent, a), "", pendingModels.Attribution{AgentID: &agent.Id, RunID: runIDPtr(runID)}); perr != nil {
						rec.Error = "could not queue this action for approval: " + perr.Error()
					} else {
						rec.Skipped = "proposed for approval — a human must approve before it runs"
						rec.Governance = govApprovalRequired
						proposed = append(proposed, actionLabel(a))
					}
					break
				}
				// Generic irreversibility backstop: a write that a person cannot
				// undo afterwards must not auto-run unattended, even in
				// full-autonomy mode. Two kinds qualify and the predicate owns the
				// distinction — a tool a remote MCP server marks destructive
				// (delete/drop/overwrite/force-push), and one of OUR OWN tools whose
				// effect leaves the workspace for good (gmail_send,
				// calendar_create_event, github_comment). It is queued for human
				// approval like governed mode, so no autonomy level can silently
				// cause irreversible damage on ANY connector — ours included, which
				// is the half this backstop used to miss: it asked only what remote
				// servers declared, and no built-in tool declares anything, so three
				// tools documented "Requires confirmation" were auto-running without
				// any. Opt out with AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN=true.
				if unattendedApprovalRequired(a.ToolName) {
					if _, perr := aiBusiness.CreatePendingAction(loopCtx, agent.CreatedBy, "agent", agent.Id.String(), a.ToolName, a.Params, proposalDescription(agent, a), "", pendingModels.Attribution{AgentID: &agent.Id, RunID: runIDPtr(runID)}); perr != nil {
						rec.Error = "could not queue this destructive action for approval: " + perr.Error()
					} else {
						rec.Skipped = "destructive action proposed for approval — a human must approve before it runs"
						rec.Governance = govApprovalRequired
						proposed = append(proposed, actionLabel(a))
					}
					break
				}
				// Consume the concurrent pre-pass result when this exact call was
				// already fetched above; otherwise execute it here as before. The
				// pre-pass only ever holds read-only calls that reached this point
				// unchanged, so nothing above is skipped by taking this branch.
				// RECORD BEFORE ACTING. Everything past this point can have an
				// effect the world keeps, and until now the only record of it
				// lived in memory until the run finished. A process that died
				// mid-run left a sent message or a posted comment with nothing
				// anywhere saying so, which is exactly why the evidence pack
				// could not claim completeness. See agentActionLog.go.
				//
				// Fails closed: an action we cannot record is an action we do
				// not take. It surfaces as a tool error, which the loop already
				// handles, rather than as a silent gap in the ledger.
				intentID, ierr := recordActionIntent(loopCtx, runIDPtr(runID), agent.Id, &agent.CreatedBy, a.ToolName, a.Params)
				if ierr != nil {
					// Falls through to the shared bookkeeping below rather than
					// jumping the loop: the model still has to be told this call
					// did not happen, or it retries blindly or narrates a success
					// that never occurred.
					rec.Error = ierr.Error()
					break
				}

				res, meta, eerr := "", map[string]string(nil), error(nil)
				if pf, ok := prefetched[sig]; ok {
					res, meta, eerr = pf.result, pf.meta, pf.err
				} else {
					res, meta, eerr = exec(loopCtx, a, userUUID)
				}
				if eerr != nil {
					rec.Error = eerr.Error()
					closeActionIntent(loopCtx, intentID, "error", eerr.Error())
				} else {
					closeActionIntent(loopCtx, intentID, "ok", "")
					rec.Result = res
					priorResults[ai.ActionSignature(a)] = res
					out.FreshCalls++
					// A fire-and-forget/background tool (e.g. code_pr) can mark its
					// ack as the agent's FINAL reply, so the run ends with that
					// honest message instead of the model fabricating a "Done."
					if meta != nil && meta[ai.MetaAgentFinal] == "true" {
						terminalReply = res
						terminalFired = true
					}
				}
			}
			step.ToolCalls = append(step.ToolCalls, rec)

			// Partition by real outcome so a user-facing disclosure is honest:
			// a tool counts as "succeeded" only when it executed and returned
			// without error; an errored call is tracked separately so a durable
			// caller can surface failures the model's prose may have glossed over.
			switch {
			case rec.Error != "":
				if !toolErrSeen[a.ToolName] {
					toolErrSeen[a.ToolName] = true
					failedTools = append(failedTools, a.ToolName)
				}
			case rec.Skipped == "" && rec.Result != "":
				if !toolOkSeen[a.ToolName] {
					toolOkSeen[a.ToolName] = true
					toolsSucceeded = append(toolsSucceeded, a.ToolName)
				}
			}

			// Feed a compact observation back to the model. Large tool outputs
			// (a commit/file/issue list from an MCP server can be many KB) are
			// truncated before they re-enter the conversation: otherwise every
			// SUBSEQUENT call in the loop re-sends the full blob, inflating the
			// request until it trips the provider's per-minute token cap (e.g.
			// Groq's 12k TPM 413). Keeping a bounded, leading slice preserves the
			// salient result for the model while keeping the request small.
			obs := truncateObservation(rec.Result)
			if rec.Error != "" {
				obs = "error: " + rec.Error
			} else if repeatOf != "" {
				obs = repeatedCallObservation(repeatOf)
			} else if rec.Skipped != "" {
				obs = "skipped: " + rec.Skipped
			}
			resultLines = append(resultLines, fmt.Sprintf("%s -> %s", a.ToolName, obs))
			obsList = append(obsList, obs)
		}
		steps = append(steps, step)

		// A terminal tool ran (e.g. code_pr enqueued its background job): end the
		// run now with the tool's honest ack as the reply, so the model can't
		// append a fabricated "Done." The real result is posted later by the
		// background worker to this same thread.
		if terminalFired {
			return finalize(model.RunSucceeded, strings.TrimSpace(terminalReply), "")
		}

		// Continue the loop: show the model its own turn + the tool results. On
		// the native path this MUST be an assistant message carrying the
		// structured tool_calls followed by one tool-role message per call (keyed
		// by tool_call_id), so the provider accepts the follow-up; on the text
		// path it is the assistant text + a single user "Tool results:" turn.
		if useNative {
			messages = append(messages, ai.ChatMessage{Role: "assistant", Content: answer, ToolCalls: nativeCalls})
			for idx, a := range actions {
				id := ""
				if idx < len(nativeCalls) {
					id = nativeCalls[idx].ID
				}
				obs := ""
				if idx < len(obsList) {
					obs = obsList[idx]
				}
				messages = append(messages, ai.ChatMessage{Role: "tool", ToolCallID: id, Name: a.ToolName, Content: obs})
			}
		} else {
			messages = append(messages, ai.ChatMessage{Role: "assistant", Content: answer})
			messages = append(messages, ai.ChatMessage{
				Role: "user",
				Content: ai.ToolResultsTurn(strings.Join(resultLines, "\n"),
					"Continue if more steps are needed, otherwise reply with a short final summary and no tool call."),
			})
		}
		finalText = cleanText

		// Checkpoint the conversation so a pause/crash resumes mid-conversation.
		checkpoint()
		// Report progress (tools used so far) to a live status surface.
		reportProgress()

		if maxRunTokens > 0 && runTokens >= int64(maxRunTokens) {
			if !skipBoundedSummary(ctx, out.FreshCalls) {
				finalText = forceSummarize(finalText)
			}
			return finalizeStopped(StopReasonRunTokenLimit, finalText, "reached the per-run token limit")
		}
	}

	if !skipBoundedSummary(ctx, out.FreshCalls) {
		finalText = forceSummarize(finalText)
	}
	return finalizeStopped(StopReasonStepLimit, finalText, "reached the step limit")
}

// toSet builds a lookup set from a slice, ignoring blanks.
func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	s := make(map[string]bool, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it != "" {
			s[it] = true
		}
	}
	return s
}

// outsideScope reports whether an action targets a channel/project the agent's
// scope does not allow. An empty scope set imposes no restriction. Only the
// common targeting params are checked; tools without a channel/project target
// are never blocked by scope.
func outsideScope(a ai.ProposedAction, channels, projects map[string]bool) bool {
	if len(channels) > 0 {
		if cid := strings.TrimSpace(a.Params["channel_uuid"]); cid != "" && !channels[cid] {
			return true
		}
	}
	if len(projects) > 0 {
		if pid := strings.TrimSpace(a.Params["project_uuid"]); pid != "" && !projects[pid] {
			return true
		}
	}
	return false
}

// agentMaxRunTokens is the per-run token ceiling: once a single run's metered
// token spend reaches this, the run stops cleanly with its progress so far.
// 0 disables the cap. Configurable via AI_AGENT_MAX_RUN_TOKENS (default 80000).
func agentMaxRunTokens() int {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_MAX_RUN_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 80000
}

// agentRunTimeout is the wall-clock deadline for a whole run, so a slow model
// or a stuck tool loop can't pin a worker forever. Configurable via
// AI_AGENT_RUN_TIMEOUT_SECONDS (default 5 minutes).
func agentRunTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_RUN_TIMEOUT_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

// proposalDescription builds the human-facing label for a write an
// approval-mode agent proposes, so the approval card reads as "<Agent>: <what
// it wants to do>". Falls back to the tool name when the model gave no
// description.
func proposalDescription(agent *model.AiAgent, a ai.ProposedAction) string {
	name := strings.TrimSpace(agent.Name)
	if name == "" {
		name = "Agent"
	}
	return name + ": " + actionLabel(a)
}

// actionLabel is the bare human-readable description of a proposed action (the
// model's description, or the tool name as a fallback), without the agent-name
// prefix — used in the in-channel disclosure where the author is already the
// agent.
func actionLabel(a ai.ProposedAction) string {
	d := strings.TrimSpace(a.Description)
	if d == "" {
		d = strings.TrimSpace(a.ToolName)
	}
	return d
}

// unattendedApprovalRequired is the whole backstop decision for one tool: does a
// human have to approve this before it runs, given that nobody is watching?
//
// Extracted so the decision can be exercised directly. Inlined at the call site it
// sat inside a long switch inside a loop that needs a database, a model and a live
// agent to reach — meaning the ONE condition standing between full autonomy and an
// irreversible action was the least testable line in the file, and the opt-out below
// had no test at all despite inverting it disabling the backstop entirely.
func unattendedApprovalRequired(toolName string) bool {
	return ai.ToolNeedsHumanBeforeUnattended(toolName) && !destructiveAutorunAllowed()
}

// destructiveAutorunAllowed reports whether the deployment has opted OUT of the
// destructive-action approval backstop (AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN),
// letting server-declared destructive MCP tools auto-run unattended. Off by
// default so irreversible external actions always require human approval.
func destructiveAutorunAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AI_ALLOW_AGENT_DESTRUCTIVE_AUTORUN"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// planStepLabel is the bare description of a gathered plan step (for the
// in-channel "proposed N steps" disclosure).
func planStepLabel(s aiBusiness.PlanStep) string {
	d := strings.TrimSpace(s.Description)
	if d == "" {
		d = strings.TrimSpace(s.ToolName)
	}
	return d
}

// agentObservationMaxChars bounds a single tool result fed back into the loop
// (~500 tokens). Tunable; chosen to keep the salient head of a result while
// preventing multi-KB tool outputs (a 40+ tool MCP server can return large
// commit/issue/file blobs, and an error payload can be huge) from re-inflating
// EVERY later request and tripping a tight provider per-minute token cap
// (e.g. Groq's 12k TPM). Lowered from 4000 to keep MCP-heavy runs under budget.
const agentObservationMaxChars = 2000

// truncateObservation caps a tool result before it re-enters the conversation,
// so a large output (e.g. an MCP commit/file/issue list) doesn't re-inflate
// every subsequent loop request and trip the provider's per-minute token cap.
// It keeps the leading slice (where the salient data usually sits) and marks
// the cut. Model/provider agnostic.
func truncateObservation(s string) string {
	if len(s) <= agentObservationMaxChars {
		return s
	}
	return s[:agentObservationMaxChars] + "\n…[truncated; the full result was longer — refine your query or ask for a specific part if you need more]"
}
