package business

// Code-PR runtime dispatch — the seam that turns "@mention the agent to make a
// change" (or a coding task assignment) into a durable, background coding job
// that clones, edits, verifies, and opens a reviewable pull request via the
// trusted codepr.Orchestrator. It rides the EXISTING durable agent-task queue
// (ai_agent_tasks): a code_pr job is just another source_type, so it inherits
// crash-safety, leasing, dedupe, retries, and the reply-surface machinery for
// free. The only new behavior is the worker branch that runs the orchestrator
// instead of the normal tool loop.
//
// Fully gated: the code_pr TOOL is hidden unless code_pr is enabled, and the
// worker branch only fires for source_type == codepr.TaskSourceType — so no
// existing job type is affected. When the feature is off or no runner is
// deployed, a job degrades to an honest "not available" outcome (it never
// panics or blocks the queue).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	codepr "github.com/akashc777/OneCamp/business/CodePR"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// agentRunSurfaceKey carries the FULL reply surface of a run (channel + post, or
// chat + message) so a tool executor can enqueue a durable follow-on job that
// posts back to the same thread. WithAgentRunScope only carries the ids the
// memory tools need; this carries the whole descriptor.
type agentRunSurfaceKey struct{}

// WithAgentRunSurface attaches the run's full reply surface. Safe with a zero
// surface (the executor then falls back to the run scope's channel/group).
func WithAgentRunSurface(ctx context.Context, s Surface) context.Context {
	return context.WithValue(ctx, agentRunSurfaceKey{}, s)
}

// agentRunSurfaceFromCtx returns the run's full surface if present, else a
// best-effort surface reconstructed from the run scope (channel/group id, no
// thread anchor), else a zero surface.
func agentRunSurfaceFromCtx(ctx context.Context) Surface {
	if v, ok := ctx.Value(agentRunSurfaceKey{}).(Surface); ok && validSurfaceKind(v.Kind) {
		return v
	}
	scope := agentRunScopeFromCtx(ctx)
	switch {
	case scope.ChannelID != "":
		return Surface{Kind: SurfaceChannelPost, ChannelID: scope.ChannelID}
	case scope.GroupID != "":
		return Surface{Kind: SurfaceGroupChat, GroupID: scope.GroupID}
	}
	return Surface{Kind: SurfaceTask}
}

// codePRSurface maps the AIAgent reply-surface descriptor onto the leaf
// codepr.Surface the orchestrator/dedupe reason about (codepr never imports
// AIAgent, so the mapping lives here).
func codePRSurface(s Surface) codepr.Surface {
	switch s.Kind {
	case SurfaceChannelPost:
		return codepr.Surface{Kind: codepr.SurfaceChannelPost, ChannelID: s.ChannelID, PostID: s.PostID}
	case SurfaceGroupChat:
		return codepr.Surface{Kind: codepr.SurfaceGroupChat, ChannelID: s.GroupID, MessageID: s.MessageID}
	case SurfaceDM:
		return codepr.Surface{Kind: codepr.SurfaceDM, MessageID: s.MessageID}
	default:
		return codepr.Surface{Kind: codepr.SurfaceTask}
	}
}

// EnqueueCodePRTask enqueues a durable background coding job that ends in a PR.
// It coalesces with any open job for the same triggering surface/instruction
// (via codepr.SourceID + the queue's ON CONFLICT), so a duplicate @mention or a
// retry never opens two conflicting PRs. Returns created=false when an open job
// already existed. agentID may be uuid.Nil (an assistant/API trigger with no
// agent); owner is the person the change is for, whose GitHub account pushes
// it; triggeredBy (optional) is the person who asked, notified as it goes, and
// the account a follow-up by someone else pushes with. MaxAttempts is 1: a
// coding run reaches a terminal, honest outcome, and must never auto-retry into
// a duplicate PR.
func EnqueueCodePRTask(ctx context.Context, agentID, owner uuid.UUID, triggeredBy *uuid.UUID, surface Surface, instruction string) (bool, error) {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return false, fmt.Errorf("a coding instruction is required")
	}
	enc, _ := EncodeSurface(surface)
	t := &model.AgentTask{
		AgentId:     agentID,
		SourceType:  codepr.TaskSourceType,
		SourceId:    codepr.SourceID(codepr.RepoRef{}, codePRSurface(surface), instruction),
		Prompt:      instruction,
		RunAsUserId: &owner,
		TriggeredBy: triggeredBy,
		Surface:     enc,
		MaxAttempts: 1,
	}
	id, created, err := model.EnqueueAgentTask(ctx, t)
	if err == nil {
		// The agent just told the thread "on it": begin the coding job now, not
		// on the next tick, and let the surface show the work immediately.
		WakeAgentTaskWorker()
		publishAgentWorkChanged(ctx, id)
	}
	return created, err
}

// executeCodePRTool is the code_pr tool executor: it does NOT run the (long)
// coding job inline — it enqueues a durable background job and returns an honest
// "on it" acknowledgement, so the agent's turn stays fast and the PR arrives in
// the thread when ready. Gated on the feature being enabled. Recovers the acting
// agent + reply surface from the run context, or from the approved proposal it
// is executing (best-effort). The job is for the person who asked, and pushes
// with their GitHub account (codePRFor).
func executeCodePRTool(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	if !ai.CodePREnabled() {
		return "Code pull requests aren't enabled in this workspace. An admin can turn them on under AI settings.", nil, nil
	}
	instruction := strings.TrimSpace(action.Params["instruction"])
	if instruction == "" {
		return "", nil, fmt.Errorf("please provide a clear instruction describing the change to make")
	}
	// A structured `repo` param ("owner/name") gives the model a reliable slot to
	// name the target repo instead of burying it in prose. We fold a validated
	// value into the instruction as a canonical leading line so the durable
	// resolver (which parses owner/name from the instruction) picks it up
	// deterministically — and the coding prompt gains clear repo context. An
	// unparseable value is ignored (never corrupts the instruction); access is
	// still verified per-run downstream.
	instruction = codePRComposeInstruction(action.Params["repo"], instruction)
	owner, err := codePRFor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	agentID, surface := codePRRunContext(ctx, action)
	// The person the change is for asked for it: they are told when the pull
	// request is ready or the run needs them.
	triggeredBy := &owner

	created, eerr := EnqueueCodePRTask(ctx, agentID, owner, triggeredBy, surface, instruction)
	if eerr != nil {
		helpers.LogErrorWithContext(ctx, "codepr: enqueue tool job failed: %v", eerr)
		return "", nil, fmt.Errorf("couldn't queue the coding task right now")
	}
	// Mark this the agent's FINAL reply for the turn: code_pr is a background
	// job, so the honest "on it / already on it" ack must stand as the response —
	// otherwise the model tends to fabricate a premature "Done, completed the
	// change" when in fact nothing has run yet. The real outcome (PR link or an
	// honest failure) is posted to this same thread by the background worker.
	terminal := map[string]string{ai.MetaAgentFinal: "true"}
	if !created {
		return "I'm already working on that change — I'll post the pull request here as soon as it's ready.", terminal, nil
	}
	return "On it — I'll make the change in an isolated sandbox, verify it against the repo's build and tests, and open a pull request here for you to review. I'll post the link when it's ready.", terminal, nil
}

// codePRFor is the person a code change is for, and so the GitHub account it is
// pushed with: the person who asked for the run when that is not the agent's
// sponsor, otherwise the person executing the call (the sponsor in their own or
// an unattended run, or whoever approved the proposal). Never the sponsor on
// someone else's behalf, which is how a teammate's request used to push with
// the sponsor's credential.
func codePRFor(ctx context.Context, actingUserUUID string) (uuid.UUID, error) {
	who := actingUserUUID
	if requester, _, forOther := ai.RunRequester(ctx); forOther {
		who = requester
	}
	id, err := uuid.Parse(strings.TrimSpace(who))
	if err != nil {
		return uuid.Nil, fmt.Errorf("couldn't tell who this change is for, so no pull request was started")
	}
	return id, nil
}

// codePRRunContext recovers the agent and the reply thread a code_pr call
// belongs to. In a live run both are on the context. For an approved proposal
// they are not: the agent comes from the stored proposal, and the thread from
// the surface the runner stamped on it when it proposed (ProposalSurfaceParam),
// which is trusted only on that path.
func codePRRunContext(ctx context.Context, action ai.ProposedAction) (uuid.UUID, Surface) {
	if aid := ai.AgentBudgetID(ctx); aid != "" {
		agentID, _ := uuid.Parse(aid)
		return agentID, agentRunSurfaceFromCtx(ctx)
	}
	if aid, ok := ai.ApprovedAgentProposal(ctx); ok {
		agentID, _ := uuid.Parse(aid)
		surface := Surface{Kind: SurfaceTask}
		if raw := strings.TrimSpace(action.Params[ai.ProposalSurfaceParam]); raw != "" {
			surface = DecodeSurface(raw)
		}
		return agentID, surface
	}
	return uuid.Nil, agentRunSurfaceFromCtx(ctx)
}

// codePRNeedsApproval reports whether a code_pr call must wait for a person:
// always under approval or plan autonomy, and otherwise unless the deployment
// opted out of the external-effect backstop (code_pr is ExternalEffect).
func codePRNeedsApproval(autonomy string) bool {
	return autonomy == model.AutonomyApproval || autonomy == model.AutonomyPlan || unattendedApprovalRequired(codePRToolName)
}

// codePRProposal is the approval request for a code_pr call: proposed to the
// person whose GitHub account the change would be pushed with (the asker, or
// the sponsor when nobody else asked), carrying the thread the run is working
// in so the pull request is posted back there once approved. A model-supplied
// value for that parameter is replaced, never trusted.
func codePRProposal(ctx context.Context, agent *model.AiAgent, a ai.ProposedAction) (uuid.UUID, map[string]string, error) {
	approver, err := codePRFor(ctx, agent.CreatedBy.String())
	if err != nil {
		return uuid.Nil, nil, err
	}
	params := make(map[string]string, len(a.Params)+1)
	for k, v := range a.Params {
		if k != ai.ProposalSurfaceParam {
			params[k] = v
		}
	}
	if enc, eerr := EncodeSurface(agentRunSurfaceFromCtx(ctx)); eerr == nil {
		params[ai.ProposalSurfaceParam] = enc
	}
	return approver, params, nil
}

// codePRRepoParamRe validates a `repo` tool param as a GitHub "owner/name" slug
// (owner: alphanumeric with internal hyphens; name: alphanumeric plus - _ .).
// Anchored so a path or URL never sneaks in — those, if the model sends them,
// are simply ignored here and only an exact owner/name is honored.
var codePRRepoParamRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

// codePRComposeInstruction folds a validated `repo` param into the instruction
// as a canonical leading line, so the durable repo resolver finds the target
// deterministically and the coding prompt carries explicit repo context. A
// missing/blank/unparseable param returns the instruction unchanged (the
// resolver then falls back to any owner/name mentioned in the prose, or the
// linked repo). Pure.
func codePRComposeInstruction(repoParam, instruction string) string {
	repo := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(repoParam), ".git"))
	if repo == "" || !codePRRepoParamRe.MatchString(repo) {
		return instruction
	}
	// Avoid duplicating the line if the model already led with it.
	if strings.HasPrefix(instruction, "Target repository: "+repo) {
		return instruction
	}
	return "Target repository: " + repo + ".\n\n" + instruction
}

// RegisterCodePRExecutor wires the code_pr tool to its executor. Called once at
// startup (StartAgentTaskWorker). Safe to call when the feature is disabled —
// the tool is simply never offered to the model until an admin enables it.
func RegisterCodePRExecutor() {
	ai.RegisterExecutor("code_pr", executeCodePRTool)
}

// runCodePRTask is the durable-worker branch for a code_pr job: it runs the
// trusted orchestrator (resolve repo → budget → isolated runner → scope judge →
// open PR → audit) and maps the outcome onto the durable state machine. It is
// invoked ONLY for source_type == codepr.TaskSourceType, so no other job type is
// affected. Honest + terminal: a success opens a PR (done), a needs-human pause
// parks awaiting a reply, and any other result is finalized done with an honest
// message (never auto-retried into a duplicate PR).
// progress, when not nil, edits the run's one living status message; it is nil
// where each update would be a new message, so stages are never spammed.
func runCodePRTask(ctx context.Context, agent *model.AiAgent, t *model.AgentTask, lease uuid.UUID, surface Surface, postStatus, progress, notifyTrigger func(string)) {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil || settings == nil || !settings.CodePREnabled || strings.TrimSpace(settings.CodePRRunnerURL) == "" {
		msg := "I can't make this change right now — code pull requests aren't enabled or no coding runner is configured. An admin can set this up under AI settings."
		postStatus(msg)
		notifyTrigger(msg)
		commitAgentTaskState(ctx, t.Id, "finish (code_pr not configured)", func(c context.Context) error {
			return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", "code_pr disabled or runner unconfigured", nil)
		})
		return
	}

	owner := uuid.Nil
	if t.RunAsUserId != nil {
		owner = *t.RunAsUserId
	}
	task := codepr.Task{
		AgentID:          t.AgentId,
		AgentTaskID:      t.Id,
		OwnerUserID:      owner,
		Instruction:      t.Prompt,
		Surface:          codePRSurface(surface),
		AgentDisplayName: agent.Name,
	}
	followUpOnCodePR(ctx, t, &task)
	if t.TriggeredBy != nil {
		task.TriggeredBy = *t.TriggeredBy
	}

	orch := codepr.NewLiveOrchestrator(codepr.LiveDeps{
		Settings: settings,
		RunID:    t.Id.String(),
	})
	if orch == nil {
		commitAgentTaskState(ctx, t.Id, "finish (code_pr orchestrator unavailable)", func(c context.Context) error {
			return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", "code_pr orchestrator unavailable", nil)
		})
		return
	}

	if progress != nil {
		orch.OnStage = func(u codepr.StageUpdate) {
			if msg := codepr.StageMessage(u); msg != "" {
				progress(msg)
			}
		}
	}
	out := orch.Run(ctx, task)
	result := codepr.FormatToolResult(out)
	helpers.LogInfoWithContext(ctx, "codepr task %s: %s", t.Id, codepr.AuditFields(out))

	// Stopped by a person: honour it before the retry/await/finish arms below, so
	// a stopped coding run is never re-dispatched (which could open a second PR)
	// and the thread gets one honest message with whatever the run had produced
	// (a pushed branch is worth reporting).
	if settleStoppedAgentWork(ctx, t, lease, result, nil, postStatus, notifyTrigger) {
		return
	}

	// A runner that is DOWN/misconfigured/egress-blocked (StatusUnavailable)
	// produced nothing — no branch, no PR — so retrying is safe (it can never
	// create a duplicate). Park it for a short, budget-free retry the claim loop
	// picks up automatically, bounded by the same transient window the tool-loop
	// path uses. We stay QUIET on these retries (no status edit, no ping) so a
	// brief runner blip doesn't spam the thread; only the terminal outcome (a PR,
	// or the honest give-up past the window) is posted below.
	if out.Status == codepr.StatusUnavailable && time.Since(t.CreatedAt) <= maxTransientRetryWindow {
		commitAgentTaskState(ctx, t.Id, "await (coding runner unavailable)", func(c context.Context) error {
			return model.AwaitAgentTask(c, t.Id, lease, 2*time.Minute, "transient: coding runner unavailable", nil)
		})
		return
	}

	postStatus(result)
	notifyTrigger(result)

	if codepr.IsAwaitingInput(out) {
		// A clean needs-human pause: park awaiting a reply on the thread (a human
		// answer re-queues it). Long floor; the resume path re-queues sooner.
		commitAgentTaskState(ctx, t.Id, "await (code_pr blocked on a human)", func(c context.Context) error {
			return model.AwaitAgentTask(c, t.Id, lease, 24*time.Hour, "blocked: "+out.NeedsHumanQuestion, nil)
		})
		return
	}
	// Terminal either way, so it never retries into a duplicate PR (failed is
	// as terminal as done). The state says which: done only when a PR opened.
	// Every other outcome used to be stored as done too, so the agent's record
	// and scorecard counted runs that produced nothing as finished work.
	state, errText := codePRTerminalState(out)
	commitAgentTaskState(ctx, t.Id, "finish (code_pr terminal)", func(c context.Context) error {
		return model.FinishAgentTask(c, t.Id, lease, state, result, errText, nil)
	})
}

// codePRTerminalState maps a finished coding run to the job's final state:
// done when a pull request opened, failed (with the reason) otherwise. Pure.
func codePRTerminalState(out codepr.Outcome) (string, string) {
	if out.Status == codepr.StatusOK {
		return model.TaskDone, ""
	}
	reason := out.Status
	if out.StopReason != "" {
		reason += ": " + out.StopReason
	}
	return model.TaskFailed, "code_pr " + reason
}

// Seams for followUpOnCodePR.
var (
	firstJobForSource = model.FirstAgentTaskForSource
	openedPRForSource = aiModels.LatestOpenedCodePRForSource
)

// followUpOnCodePR prepares a follow-up job (one that is not the first for its
// thread or task) to build on the earlier work: the run is told the original
// request as well as the follow-up, and is pointed at the pull request the
// earlier work opened so it can add a commit there rather than open another.
// The orchestrator still checks that pull request with GitHub before pushing.
// Best-effort: a lookup failure leaves a plain fresh run.
func followUpOnCodePR(ctx context.Context, t *model.AgentTask, task *codepr.Task) {
	firstID, original, found, err := firstJobForSource(ctx, t.AgentId, t.SourceType, t.SourceId)
	if err != nil || !found || firstID == t.Id {
		return
	}
	task.Instruction = codepr.ContinuationInstruction(original, t.Prompt)
	pr, err := openedPRForSource(ctx, t.AgentId, t.SourceType, t.SourceId)
	if err != nil || pr == nil {
		return
	}
	task.Continue = &codepr.PriorPR{
		URL:        pr.PRURL,
		Repo:       codepr.RepoRef{Owner: pr.RepoOwner, Name: pr.RepoName},
		HeadBranch: pr.HeadBranch,
		BaseBranch: pr.BaseBranch,
	}
}
