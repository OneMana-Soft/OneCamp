package business

// agentTaskWorker drives the durable ai_agent_tasks queue: it leases runnable
// jobs, executes each through the proven runner (RunAgent — owner envelope,
// per-tool permission re-checks, token caps, autonomy modes all still apply),
// and posts the outcome back to the originating surface (v1: as a comment on
// the assigned project task). It is the piece that makes a hand-off DURABLE:
//   - survives restarts (a leased job whose worker died is reclaimed + retried)
//   - retries transient faults with backoff WITHOUT burning the failure budget
//   - pauses (awaiting_input) on a token-budget exhaustion or a needs-a-human
//     blocker, resuming when re-queued, again without burning retries
//   - never goes silent: an "On it" ack, the result, or an honest failure note
//     is posted to the task so the team always knows where the AI teammate is.
//
// It reuses the same primitives as the schedule loop (a ticker + the AI
// enablement gate + KeyedLock-free bounded concurrency) and adds no parallel
// model runtime.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// agentTaskTickInterval is how often the worker wakes to claim due jobs.
	agentTaskTickInterval = 5 * time.Second
	// agentTaskLeaseMargin is the head-room added to the LONGEST possible run
	// wall clock to get the lease TTL (see agentTaskLeaseTTL). It absorbs the
	// non-run time a leased job spends on posting status, resolving principals,
	// and DB round trips, so the lease always outlives a healthy run.
	agentTaskLeaseMargin = 5 * time.Minute
	// agentTaskLeaseMinTTL / agentTaskLeaseMaxTTL bound an operator override of
	// the lease TTL. The derived floor still wins over agentTaskLeaseMaxTTL: a
	// lease shorter than the work it guards is the bug, not a tuning choice.
	agentTaskLeaseMinTTL = 2 * time.Minute
	agentTaskLeaseMaxTTL = 2 * time.Hour
	// agentTaskLeaseEnvVar overrides the derived lease TTL, in seconds. It can
	// only ever LENGTHEN the lease past the derived floor.
	agentTaskLeaseEnvVar = "AI_AGENT_TASK_LEASE_SECONDS"
	// agentTaskHeartbeatMin / agentTaskHeartbeatMax bound the renewal interval
	// derived from the TTL (agentTaskHeartbeatDivisor), so a heartbeat is
	// frequent enough to prove ownership well before expiry without hammering
	// the DB on a long run.
	agentTaskHeartbeatDivisor = 4
	agentTaskHeartbeatMin     = 15 * time.Second
	agentTaskHeartbeatMax     = 2 * time.Minute
	// agentTaskCommitAttempts / agentTaskCommitBackoff bound the retry of a
	// durable state transition (finish / retry / await / checkpoint) when the DB
	// is momentarily unhappy. A dropped terminal transition is what leaves a job
	// "working" until its lease expires and then replays the WHOLE job.
	agentTaskCommitAttempts = 3
	agentTaskCommitBackoff  = 400 * time.Millisecond
	// defaultAgentTaskConcurrency is the workspace-wide ceiling on in-flight
	// agent tasks (across all teammates), protecting the model rate limit / DB
	// pool. Override with AI_AGENT_TASK_CONCURRENCY. The per-run token + step
	// caps still apply to each task.
	defaultAgentTaskConcurrency = 6
	// defaultAgentTaskPerAgent is how many tasks ONE AI teammate may run at
	// once, so an agent multitasks like a real teammate while staying fair to
	// the others. Override with AI_AGENT_TASK_PER_AGENT. 0 = no per-agent limit
	// (only the global ceiling).
	defaultAgentTaskPerAgent = 3
	// maxTransientRetryWindow bounds how long a job keeps taking budget-free
	// retries for transient stalls (circuit open / rate limit / run timeout).
	// A real transient blip clears in seconds; past this window the model is
	// effectively down, so the job is failed with an honest note rather than
	// retried forever (which would show as "running" indefinitely).
	maxTransientRetryWindow = 20 * time.Minute
)

// agentTaskConcurrency is the global in-flight ceiling (env override).
func agentTaskConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_TASK_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultAgentTaskConcurrency
}

// agentTaskPerAgentCap is how many tasks one agent may run concurrently (env
// override; 0 disables the per-agent limit).
func agentTaskPerAgentCap() int {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_TASK_PER_AGENT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultAgentTaskPerAgent
}

// agentTaskMaxRunWall is the longest wall clock ANY job this worker dispatches
// can legitimately consume: the slowest of the run kinds it drives. Today that
// is the normal tool loop (agentRunTimeout, default 5m) and a code-PR coding run
// (codepr.MaxRunWallClock = the shared coding wall ceiling + its dispatch
// overhead, default 20m). Both come from their owning package's single source of
// truth, so a change there can never silently desync the queue's timing again.
func agentTaskMaxRunWall() time.Duration {
	longest := agentRunTimeout()
	if w := codepr.MaxRunWallClock(); w > longest {
		longest = w
	}
	return longest
}

// agentTaskLeaseTTL is how long a claim stays valid before the job is treated as
// crashed and reclaimed. It is DERIVED — the longest possible run wall clock
// plus agentTaskLeaseMargin — never an independent literal: a TTL shorter than
// the work it guards means a second worker reclaims a job that is still
// executing, which duplicates work, produces conflicting pushes, and reports the
// wrong status. Live runs additionally heartbeat (RenewAgentTaskLease), so the
// TTL only has to cover the gap between renewals plus a healthy run's tail.
//
// agentTaskLeaseEnvVar can lengthen it (bounded by agentTaskLeaseMaxTTL); it can
// never shorten it below the derived floor.
func agentTaskLeaseTTL() time.Duration {
	floor := agentTaskMaxRunWall() + agentTaskLeaseMargin
	ttl := floor
	if v := strings.TrimSpace(os.Getenv(agentTaskLeaseEnvVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ttl = time.Duration(n) * time.Second
		}
	}
	if ttl < agentTaskLeaseMinTTL {
		ttl = agentTaskLeaseMinTTL
	}
	if ttl > agentTaskLeaseMaxTTL {
		ttl = agentTaskLeaseMaxTTL
	}
	if ttl < floor {
		ttl = floor // correctness beats the tuning cap
	}
	return ttl
}

// agentTaskHeartbeatInterval is how often a live run proves it still owns its
// lease: a fraction of the TTL, bounded so it is always comfortably below expiry
// (several renewals fit inside one TTL) yet never chattier than needed.
func agentTaskHeartbeatInterval(ttl time.Duration) time.Duration {
	iv := ttl / agentTaskHeartbeatDivisor
	if iv < agentTaskHeartbeatMin {
		iv = agentTaskHeartbeatMin
	}
	if iv > agentTaskHeartbeatMax {
		iv = agentTaskHeartbeatMax
	}
	return iv
}

// agentTaskLease is a worker's LIVE ownership handle on one claimed job. It
// renews the lease in the background (guarded by task id AND lease token, so
// only the current owner can extend) and, the moment a renewal proves the lease
// was lost, cancels the run context so the in-flight work stops instead of
// continuing to write on behalf of a job somebody else now owns.
//
// Generic on purpose: any durable dispatch path can wrap itself in one.
type agentTaskLease struct {
	taskID uuid.UUID
	token  uuid.UUID
	ttl    time.Duration
	lost   atomic.Bool
	// stopAsked records that a HUMAN asked for this job to stop. The heartbeat
	// learns it in the same round trip that proves ownership, then cancels the
	// run context — so a stop takes effect within one heartbeat instead of
	// waiting for a wall-clock limit. Distinct from lost: a lost lease means go
	// quiet (someone else owns the story), while a stop means finish the story
	// honestly ("stopped, here's what I'd done").
	stopAsked atomic.Bool
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   sync.Once
	wg        sync.WaitGroup
}

// startAgentTaskLease begins heartbeating a claimed job and returns the handle
// plus the RUN context to execute under. The run context is cancelled when the
// lease is lost (or when Release is called), so a reclaimed job's original
// worker unwinds promptly. Durable transitions must NOT use the run context —
// they use the caller's context via commitAgentTaskState, so a terminal outcome
// still lands after the run itself was cut short.
func startAgentTaskLease(ctx context.Context, taskID, token uuid.UUID) (*agentTaskLease, context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	l := &agentTaskLease{
		taskID: taskID,
		token:  token,
		ttl:    agentTaskLeaseTTL(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	l.wg.Add(1)
	go l.heartbeat(ctx)
	return l, runCtx
}

// heartbeat renews the lease until the job finishes, the lease is lost, or the
// process shuts down. A renewal ERROR is transient (DB blip) and simply retried
// on the next tick — the run keeps going. A renewal that matches NO ROW is
// proof of lost ownership: it cancels the run and stops.
func (l *agentTaskLease) heartbeat(ctx context.Context) {
	defer l.wg.Done()
	t := time.NewTicker(agentTaskHeartbeatInterval(l.ttl))
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			held, stopAsked, err := model.RenewAgentTaskLease(ctx, l.taskID, l.token, l.ttl)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "agentTaskWorker: lease renewal failed (task=%s), will retry: %v", l.taskID, err)
				continue
			}
			if held && stopAsked {
				// A person asked this to stop. Unwind the run now (the tool loop
				// sees a cancelled context and returns), but keep the lease: this
				// worker still owns the job and must write the honest terminal
				// row, including whatever the agent had already done.
				l.stopAsked.Store(true)
				helpers.LogInfoWithContext(ctx, "agentTaskWorker: stop requested by a person, unwinding run (task=%s)", l.taskID)
				l.cancel()
				return
			}
			if held {
				continue
			}
			// Someone else owns this job now (our lease expired and it was
			// reclaimed, or it was settled elsewhere). Stop executing rather
			// than racing the new owner into duplicate work.
			l.lost.Store(true)
			helpers.MessageLogs.ErrorLog.Printf("agentTaskWorker: lease lost, abandoning run (task=%s)", l.taskID)
			l.cancel()
			return
		}
	}
}

// Lost reports whether the lease was proven gone. Callers use it to go quiet:
// no status edits, no notifications, no writes for a job they no longer own.
func (l *agentTaskLease) Lost() bool {
	return l != nil && l.lost.Load()
}

// StopAsked reports whether a human requested this job be stopped. The lease is
// still held, so the caller SHOULD post the final "stopped" message and record
// the terminal row — unlike a lost lease, this story is still ours to close.
func (l *agentTaskLease) StopAsked() bool {
	return l != nil && l.stopAsked.Load()
}

// Release stops the heartbeat and cancels the run context. Idempotent; safe to
// defer.
func (l *agentTaskLease) Release() {
	if l == nil {
		return
	}
	l.stopped.Do(func() { close(l.done) })
	l.wg.Wait()
	l.cancel()
}

// commitAgentTaskState performs ONE durable, lease-guarded state transition
// reliably. Every transition (finish / retry / await / checkpoint) used to be
// called with `_ =`, so a transient DB fault silently dropped the outcome: the
// job stayed "working" until its lease expired and then the WHOLE job replayed,
// repeating writes that had already landed.
//
// It distinguishes the two failure modes that need opposite responses:
//   - no row matched (sql.ErrNoRows) => the lease is gone. Another worker owns
//     the job and will record its outcome; retrying would clobber it. Stop
//     QUIETLY (info, not error) and report false.
//   - any other error => a transient DB fault. Retry with bounded backoff, then
//     log with task context and report false.
//
// Generic: apply is any lease-guarded model call, so this is the one place the
// worker's durability policy lives. Uses a cancellation-free context so a
// terminal transition still lands when the run context was just cancelled.
func commitAgentTaskState(ctx context.Context, taskID uuid.UUID, what string, apply func(context.Context) error) bool {
	base := context.WithoutCancel(ctx)
	var last error
	for attempt := 1; attempt <= agentTaskCommitAttempts; attempt++ {
		err := apply(base)
		if err == nil {
			return true
		}
		if errors.Is(err, sql.ErrNoRows) {
			helpers.LogInfoWithContext(ctx, "agentTaskWorker: %s skipped, lease no longer held (task=%s)", what, taskID)
			return false
		}
		last = err
		if attempt < agentTaskCommitAttempts {
			select {
			case <-base.Done():
			case <-time.After(time.Duration(attempt) * agentTaskCommitBackoff):
			}
		}
	}
	helpers.LogErrorWithContext(ctx, "agentTaskWorker: %s failed after %d attempts (task=%s): %v",
		what, agentTaskCommitAttempts, taskID, last)
	return false
}

// settleAbandonedAgentTask is the reconciler that guarantees a job never sits in
// "working" because its dispatch path returned (or panicked) without recording
// an outcome. If this worker STILL holds the lease, nothing terminal landed, so
// it records one now — failing the job when its retry budget is spent, otherwise
// re-queueing it with backoff for a clean second try. Lease-guarded and
// idempotent: when the outcome DID land (or the lease moved on) the ownership
// probe is false and this is a no-op, so it can be deferred unconditionally.
func settleAbandonedAgentTask(ctx context.Context, t *model.AgentTask, lease uuid.UUID, reason string) {
	if t == nil {
		return
	}
	held, err := model.AgentTaskLeaseHeld(context.WithoutCancel(ctx), t.Id, lease)
	if err != nil || !held {
		return // settled already, reclaimed elsewhere, or the probe itself failed
	}
	// A stop the dispatch path never got to settle (it returned early, or
	// panicked) must still be honoured: re-queueing here would restart work a
	// person explicitly ended.
	if agentWorkStopRequested(ctx, t) {
		commitAgentTaskState(ctx, t.Id, "cancel (stopped, run abandoned)", func(c context.Context) error {
			return model.CancelLeasedAgentTask(c, t.Id, lease, stoppedNote(c, stopRequester(c, t)), "", t.LastRunId)
		})
		return
	}
	if t.Attempt >= t.MaxAttempts {
		commitAgentTaskState(ctx, t.Id, "finish (abandoned run)", func(c context.Context) error {
			return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", reason, t.LastRunId)
		})
		return
	}
	commitAgentTaskState(ctx, t.Id, "retry (abandoned run)", func(c context.Context) error {
		return model.RetryAgentTask(c, t.Id, lease, agentTaskRetryBackoff(t), true, reason, t.LastRunId)
	})
}

var (
	agentTaskMu      sync.Mutex
	agentTaskStarted bool
)

// StartAgentTaskWorker registers the task-assignment listener and starts the
// durable-queue worker loop. Idempotent; call once at startup after the DB is
// ready (alongside StartTriggers). Inert until a task is assigned to an agent.
func StartAgentTaskWorker(ctx context.Context) {
	agentTaskMu.Lock()
	if agentTaskStarted {
		agentTaskMu.Unlock()
		return
	}
	agentTaskStarted = true
	agentTaskMu.Unlock()

	webhookBusiness.RegisterEventListener(handleTaskAssignedForAgent)
	webhookBusiness.RegisterEventListener(handleTaskCommentForAgent)
	// Wire the code_pr tool → durable coding-job executor (gated: the tool is
	// only offered to the model when an admin has enabled code PRs).
	RegisterCodePRExecutor()
	go agentTaskLoop(ctx)
	helpers.MessageLogs.InfoLog.Println("AI agent task worker started")
}

// agentTaskWake is the event-driven kick: enqueueing (or resuming) a job signals
// it so the worker claims the job NOW instead of waiting out the tick. Buffered
// and non-blocking, so a burst collapses into one wake and no producer ever waits
// on the worker. The ticker remains the fallback — a missed signal only costs a
// few seconds, never a stuck job.
var agentTaskWake = make(chan struct{}, 1)

// WakeAgentTaskWorker asks the durable worker to look for runnable jobs
// immediately. Safe to call from anywhere (including before the worker starts,
// and from many goroutines at once); never blocks.
//
// This is what makes the durable path viable for INTERACTIVE work: a channel
// @mention that runs as a durable job would otherwise sit in the queue for up to
// a tick before the agent even began, which reads as the agent ignoring you.
func WakeAgentTaskWorker() {
	select {
	case agentTaskWake <- struct{}{}:
	default: // a wake is already pending; one is enough
	}
}

// agentTaskLoop reclaims crashed jobs and drains the runnable queue on every
// wake signal (and each tick as a fallback), with bounded concurrency.
func agentTaskLoop(ctx context.Context) {
	t := time.NewTicker(agentTaskTickInterval)
	defer t.Stop()
	sem := make(chan struct{}, agentTaskConcurrency())
	for {
		select {
		case <-ctx.Done():
			return
		case <-agentTaskWake:
			drainAgentTasks(ctx, sem)
		case <-t.C:
			drainAgentTasks(ctx, sem)
		}
	}
}

// drainAgentTasks reclaims expired leases, then claims and runs due jobs up to
// the concurrency budget. Only runs while AI is enabled (an inert workspace
// never spends a model call or churns the queue).
func drainAgentTasks(ctx context.Context, sem chan struct{}) {
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	if _, err := model.ReclaimExpiredLeases(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: reclaim leases failed: %v", err)
	}
	for {
		select {
		case sem <- struct{}{}:
		default:
			return // concurrency budget full this tick
		}
		task, err := model.ClaimNextRunnable(ctx, agentTaskLeaseTTL(), agentTaskPerAgentCap())
		if err != nil {
			<-sem
			helpers.LogErrorWithContext(ctx, "agentTaskWorker: claim failed: %v", err)
			return
		}
		if task == nil {
			<-sem
			return // nothing due
		}
		go func(t *model.AgentTask) {
			jobCtx := context.WithoutCancel(ctx)
			defer func() {
				if r := recover(); r != nil {
					helpers.MessageLogs.ErrorLog.Printf("agentTaskWorker: recovered panic (task=%s): %v", t.Id, r)
					// A recovered panic used to only log, leaving the job
					// "working" until its lease expired — minutes of a stale
					// status for the user. Record a real outcome now
					// (lease-guarded, so it can never clobber another owner).
					if t.LeaseToken != nil {
						settleAbandonedAgentTask(jobCtx, t, *t.LeaseToken, "the run stopped unexpectedly")
					}
				}
				<-sem
			}()
			runOneAgentTask(jobCtx, t)
		}(task)
	}
}

// runOneAgentTask executes one leased job and records its outcome, posting
// status to the originating task. The job has already been transitioned to
// running with attempt incremented by the claim.
func runOneAgentTask(ctx context.Context, t *model.AgentTask) {
	if t.LeaseToken == nil {
		return
	}
	lease := *t.LeaseToken

	// Announce the two moments a watcher cares about: the job STARTED, and
	// (deferred) whatever state it ended in. Between them the run's own status
	// comment carries progress, so these two pushes replace what used to be a
	// timer in every client watching the thread.
	publishAgentWorkChanged(ctx, t.Id)
	defer publishAgentWorkChanged(ctx, t.Id)

	// Hold the lease for as long as this job actually runs: the heartbeat proves
	// ownership (id + lease token) on an interval well below expiry, so a long
	// but LIVE run is never reclaimed mid-flight; and if a renewal shows the
	// lease was lost, leaseCtx is cancelled so the run unwinds instead of
	// writing on behalf of a job another worker now owns.
	leaseHold, leaseCtx := startAgentTaskLease(ctx, t.Id, lease)
	defer leaseHold.Release()
	// Whatever happens below — an early return, a dropped transition, a panic in
	// a dispatch path — the job must not be left "working". If we still hold the
	// lease when this returns, no outcome was recorded, so record one.
	defer func() {
		reason := "the run ended without recording an outcome"
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("agentTaskWorker: recovered panic in run (task=%s): %v", t.Id, r)
			reason = "the run stopped unexpectedly"
		}
		settleAbandonedAgentTask(ctx, t, lease, reason)
	}()

	// A stop that landed between the claim and here (or that raced the claim):
	// settle it before any work starts, so no tokens are spent and nothing is
	// written on behalf of a job a person just stopped.
	if t.CancelRequestedAt != nil {
		commitAgentTaskState(ctx, t.Id, "cancel (stopped before starting)", func(c context.Context) error {
			return model.CancelLeasedAgentTask(c, t.Id, lease, stoppedNote(c, stopRequester(c, t)), "", nil)
		})
		return
	}

	agent, err := model.GetAgentByID(ctx, t.AgentId)
	if err != nil || agent == nil {
		// The agent was deleted/disabled after assignment; close the job out.
		commitAgentTaskState(ctx, t.Id, "finish (agent unavailable)", func(c context.Context) error {
			return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", "the assigned AI teammate is no longer available", nil)
		})
		return
	}

	// Resolve the reply surface (the legacy task surface by default) and its
	// status poster. One evolving status comment for this run: "On it…" → live
	// "working…" edits → the final result, posted as the agent's badged
	// principal on the RIGHT surface (task comment, channel-post comment, or
	// chat comment). Falls back to a best-effort one-shot post only where one
	// exists (the task surface) when the poster can't be resolved.
	surface := DecodeSurface(t.Surface)
	var status StatusPoster
	var taskPoster *taskStatusPoster
	if surface.Kind == SurfaceTask {
		if p := newTaskStatusPoster(ctx, agent, t); p != nil {
			status = p
			taskPoster = p
		}
	} else if p := newStatusPoster(ctx, agent, surface); p != nil {
		status = p
	}
	// Both user-facing channels go quiet the moment the lease is proven lost: the
	// worker that reclaimed the job owns the story from then on, so posting here
	// would duplicate or contradict it.
	postStatusNow := func(text string) {
		if leaseHold.Lost() {
			return
		}
		if status != nil {
			status.Set(ctx, text)
			return
		}
		// No status poster could be resolved (the agent has no principal yet, or
		// the surface ids didn't parse). A durable run must still SPEAK — silence
		// after someone asked an agent for something is the worst outcome, and now
		// that ordinary @mentions run durably this fallback is the difference
		// between a degraded reply and none at all. Each surface falls back to the
		// same best-effort post the synchronous path uses.
		switch surface.Kind {
		case SurfaceTask:
			postAgentTaskStatus(ctx, agent, t, text)
		case SurfaceChannelPost:
			postAgentReply(ctx, agent, resolveAgentBot(ctx, agent), surface.ChannelID, surface.PostID, text)
		}
	}

	// notifyTrigger pings the person who asked (the @mention author) when the
	// run reaches a moment they should know about — blocked awaiting their
	// input, or finished. In-app activity + push via the shared human-mention
	// path; a no-op for the task surface (its assignee is served by the task
	// comment) and when no human triggered the run (schedule/event). Best-effort.
	triggeredBy := ""
	if t.TriggeredBy != nil {
		triggeredBy = t.TriggeredBy.String()
	}
	notifyTrigger := func(text string) {
		if leaseHold.Lost() {
			return
		}
		if surface.Kind == SurfaceTask {
			notifyTaskDelegator(agent, t.SourceId, triggeredBy, text)
			return
		}
		notifyTriggerer(ctx, agent, surface, triggeredBy, text)
	}

	// Slow ack. A durable run announces itself ("On it…") so nobody is left
	// wondering whether the agent heard them — but announcing INSTANTLY means a
	// question the agent answers in two seconds gets an "On it" comment that
	// flickers into the answer moments later. Now the ack is armed on a short
	// delay and cancelled by the first real message, so a fast run posts exactly
	// one comment (its answer) and only genuinely slow work says "On it".
	//
	// Every user-facing post goes through postStatus, which cancels the pending
	// ack first — and Cancel WAITS for an ack that is already mid-post, so a late
	// ack can never overwrite the real result.
	var ack *slowAck
	postStatus := func(text string) {
		ack.Cancel()
		postStatusNow(text)
	}
	defer func() { ack.Cancel() }()

	// Code-PR jobs run the trusted orchestrator (resolve → budget → isolated
	// runner → scope judge → open PR → audit) instead of the normal tool loop.
	// Strictly gated on the code_pr source type, so no other job type is
	// touched. It handles its own acknowledgement + terminal transition.
	if t.SourceType == codepr.TaskSourceType {
		// Acknowledge ("On it") EXACTLY once, even across a needs-human
		// pause+resume or a crash reclaim. A code_pr run never creates an
		// ai_agent_runs row, so LastRunId (used for the normal path) stays nil
		// and would re-greet on every resume; last_error is cleared by the
		// resume path, so it can't gate either. We instead stamp a durable
		// marker into the (otherwise-unused for code_pr) task messages the first
		// time we dispatch — it survives both resume and reclaim.
		// A coding job is never fast (clone → edit → verify → PR), so its ack is
		// posted immediately rather than on the slow-ack delay: the thread should
		// know a PR is coming before minutes of silence.
		if !codePRAlreadyAcked(t) {
			postStatus("On it — I'll open a pull request here when it's ready.")
			commitAgentTaskState(ctx, t.Id, "checkpoint code_pr ack", func(c context.Context) error {
				return model.SaveAgentTaskMessages(c, t.Id, lease, codePRAckMarker)
			})
		}
		// leaseCtx: a coding run is the longest thing we dispatch, so it is the
		// one that most needs to stop promptly if the lease is ever lost.
		var progress func(string)
		if status != nil {
			progress = postStatus
		}
		runCodePRTask(leaseCtx, agent, t, lease, surface, postStatus, progress, notifyTrigger)
		return
	}

	// Acknowledge in-thread ONCE, before any run has executed, so the
	// assignee/team see the AI teammate has picked it up. Gated on LastRunId
	// (set the moment RunAgent creates its run row, and preserved by every
	// requeue) rather than the attempt counter — a transient stall (circuit
	// open / run timeout / rate limit) intentionally rolls the attempt counter
	// back so it doesn't burn the retry budget, and using it here re-posted the
	// ack on every such retry (the duplicate "On it" spam).
	// Armed only when there IS an editable status surface: an ack that can be
	// edited into the result costs nothing, but in the degraded fallback (no
	// poster, so every message is a new comment) it would just be noise in front
	// of the answer.
	if !alreadyRan(t) && status != nil {
		ack = startSlowAck(agentAckDelay(), func() {
			postStatusNow("On it — I'll work on this and post back here when I'm done.")
			// Work that takes long enough to say "On it" has started, so the
			// task says so on the board too. A fast answer or question fires
			// no ack, and leaves the task where it was.
			taskPoster.MarkStarted(ctx)
		})
	}

	// Resume the durable conversation when one was saved (a prior pause/crash),
	// and checkpoint after each step so the next pause/crash resumes mid-run.
	// A fresh job (no saved messages) runs from the prompt as before.
	var resumeMsgs []ai.ChatMessage
	if strings.TrimSpace(t.Messages) != "" && strings.TrimSpace(t.Messages) != "[]" {
		if uerr := json.Unmarshal([]byte(t.Messages), &resumeMsgs); uerr != nil {
			resumeMsgs = nil // corrupt/older state: fall back to a fresh run
		}
	}
	runCtx := WithAgentResumeState(WithAnotherSession(leaseCtx, t.Sessions < maxAgentTaskSessions), resumeMsgs, func(msgs []ai.ChatMessage) {
		if b, merr := json.Marshal(msgs); merr == nil {
			commitAgentTaskState(ctx, t.Id, "checkpoint conversation", func(c context.Context) error {
				return model.SaveAgentTaskMessages(c, t.Id, lease, string(b))
			})
		}
	})
	// Conversation scope for the memory tools, from the durable job's surface,
	// so remember/forget work identically on a background run.
	if surface.Kind == SurfaceChannelPost {
		runCtx = WithAgentRunScope(runCtx, surface.ChannelID, "")
	} else if surface.Kind == SurfaceGroupChat || surface.Kind == SurfaceDM {
		runCtx = WithAgentRunScope(runCtx, "", surface.GroupID)
	}
	// Live progress: update the status comment as stages complete (throttled).
	runCtx = WithAgentProgress(runCtx, func(tools []string) {
		if status != nil {
			status.Progress(ctx, tools)
		}
	})
	// Mid-run steering: hand the runner a take-and-clear source for instructions
	// people gave while the agent was working, so a correction reaches it before
	// its next action instead of after it finished the job the wrong way. Read on
	// a detached context — the run context is exactly what gets cancelled when the
	// work is stopped, and a drained instruction must not be lost to that.
	runCtx = WithAgentSteering(runCtx, func() []string {
		return drainAgentTaskSteering(context.WithoutCancel(ctx), t.Id, lease)
	})
	// …and let the run acknowledge a note it picked up, on the same evolving
	// status comment the rest of the run speaks through (so a receipt never
	// becomes an extra comment in the thread).
	runCtx = WithAgentStatusNote(runCtx, postStatus)
	// The launch vocabulary did not survive the queue; the delegation hop did.
	// Say who started this before the runner sees the generic trigger source.
	runCtx = auditBusiness.WithInitiator(runCtx, initiatorForTask(t))

	outcome := RunAgent(runCtx, agent, "task_assignment", t.Prompt, false)
	if outcome == nil {
		// A stop unwinds the run context, so "no outcome" can also mean "a person
		// stopped it" — settle that honestly instead of retrying stopped work.
		if settleStoppedAgentWork(ctx, t, lease, "", nil, postStatus, notifyTrigger) {
			return
		}
		scheduleAgentTaskRetry(ctx, t, lease, "the run produced no outcome", nil)
		return
	}
	var runID *uuid.UUID
	if outcome.RunID != uuid.Nil {
		rid := outcome.RunID
		runID = &rid
	}

	// Stopped by a person: that decision outranks every other classification
	// below (a stop must not be retried, finalized as "done", or parked awaiting
	// input). Checked once here, so the honest "stopped" message and terminal row
	// happen exactly once whether the run stopped mid-step or had already
	// finished when the request landed. Keeps whatever the run produced.
	if leaseHold.StopAsked() || outcome.StopReason == StopReasonCanceled {
		if settleStoppedAgentWork(ctx, t, lease, outcome.Result, runID, postStatus, notifyTrigger) {
			return
		}
	}

	// A run that already performed a state-changing (write) action must be
	// FINALIZED here, never retried or left mid-flight: the write has already
	// happened, so re-running would duplicate it, strand the live "working…"
	// status comment, and risk a false "couldn't complete" once the retry
	// budget is spent. A clean success and a needs-human block are handled by
	// their own arms below; every other outcome (a fault or transient stop that
	// occurred AFTER the write landed) is finalized as done with a ground-truth
	// summary so the comment always advances to the real result.
	if outcome.Status != model.RunSucceeded &&
		!(outcome.Status == model.RunStopped && outcome.Blocked) &&
		runDidWrite(outcome) {
		finalizeCompletedWork(ctx, t, lease, outcome, runID, postStatus, notifyTrigger)
		return
	}

	switch outcome.Status {
	case model.RunSucceeded:
		body := strings.TrimSpace(outcome.Result)
		if footer := proposedApprovalFooter(outcome.Proposed); footer != "" {
			if body != "" {
				body += "\n\n" + footer
			} else {
				body = footer
			}
		}
		if body == "" {
			body = "Done."
		}
		if tf := toolsUsedFooter(outcome.ToolsSucceeded); tf != "" {
			body += "\n\n" + tf
		}
		if nf := failedToolsNote(outcome.FailedTools); nf != "" {
			body += "\n\n" + nf
		}
		// Shown with who handed it over; stored and forwarded as the agent wrote it.
		postStatus(withHandoff(ctx, agent.Id, t.DelegationChain, triggeredBy, body))
		notifyTrigger(body)
		// Make a CLEAN, FINAL answer audible to any agent it @mentions. This is
		// the only place in the durable path that emits, and deliberately so.
		//
		// The obvious-looking alternative — emitting inside StatusPoster — is
		// wrong: Set() also carries the "On it…" ack and every progress update, so
		// a mention in an intermediate note would re-fire the delegation on each
		// tick. Only a finished answer can hand work on.
		//
		// Nor do the other terminal arms emit. A blocked run is asking a HUMAN a
		// question, a stopped run was cancelled by a person, and the
		// work-already-landed finalize is a degraded outcome the agent never got to
		// reason about — none of those should be able to recruit another agent.
		//
		// This path (durable) is the DEFAULT for any agent with tools, so it is
		// where delegation actually happens; the synchronous channel reply only
		// runs for tool-less agents and as a fallback.
		emitAgentFinalAnswer(ctx, agent, surface, t.SourceId, triggeredBy, t.DelegationHop, t.DelegationChain, body)
		commitAgentTaskState(ctx, t.Id, "finish (succeeded)", func(c context.Context) error {
			return model.FinishAgentTask(c, t.Id, lease, model.TaskDone, body, "", runID)
		})

	case model.RunStopped:
		// A generic needs_human blocker is a clean pause, not a throttle/limit:
		// post the question and park awaiting a human reply (which resumes it).
		if outcome.Blocked {
			// Rebuilt from the outcome's own fields rather than re-parsed from
			// the rendered text, so what is posted, what is stored, and what a
			// reply is later matched against are all the same list.
			elic := Elicitation{
				Question: strings.TrimSpace(outcome.BlockReason),
				Options:  outcome.BlockOptions,
			}
			if elic.Question == "" {
				elic.Question = "I need a decision from someone to continue."
			}
			asked := elic.Render()
			replyHint := "\n\nReply here and I'll pick it back up."
			if len(elic.Options) > 0 {
				replyHint = "\n\nReply with one of those and I'll pick it back up."
			}
			postStatus("I'm blocked and need your input to continue:\n\n" + asked + replyHint)
			// Ping the person who asked so a blocked run doesn't wait unseen.
			notifyTrigger("I'm blocked and need your input to continue: " + asked)
			// Long park; a human reply on the task re-queues it immediately, so
			// this is just a safety floor, not the resume mechanism.
			//
			// The note carries the RENDERED question because it is what the work
			// panel shows, and a person looking at a blocked job needs to see
			// the choices there too, not only in the thread.
			commitAgentTaskState(ctx, t.Id, "await (blocked on a human)", func(c context.Context) error {
				return model.AwaitAgentTask(c, t.Id, lease, 24*time.Hour, "blocked: "+asked, runID)
			})
			return
		}
		handleStoppedAgentTask(ctx, agent, t, lease, outcome, runID, postStatus, notifyTrigger)

	default: // RunFailed (or unknown)
		// A genuine fault: retry with backoff, giving up (with an honest note)
		// once the retry budget is exhausted.
		if t.Attempt >= t.MaxAttempts {
			postStatus("I wasn't able to complete this after a few attempts. Someone may need to take a look or re-assign it to me once it's unblocked.")
			commitAgentTaskState(ctx, t.Id, "finish (retries exhausted)", func(c context.Context) error {
				return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", helpers.FirstNonBlank(outcome.Error, "run failed"), runID)
			})
			return
		}
		scheduleAgentTaskRetry(ctx, t, lease, helpers.FirstNonBlank(outcome.Error, "run failed"), runID)
	}
}

// stopDisposition is the durable action the worker takes for a RunStopped
// outcome. Deriving it is pure logic (classifyStop), so it is unit-tested
// without a DB; the worker then performs the matching state transition.
type stopDisposition int

const (
	stopAwaitBudget     stopDisposition = iota // pause; resumes after the cap resets
	stopRetryTransient                         // free retry (no budget cost)
	stopFinalizePartial                        // bounded completion: keep partial, done
)

// classifyStop maps a RunStopped outcome to its durable disposition. It switches
// on the runner's STABLE StopReason code; a blank code (older/unknown stop)
// falls back to matching the human-readable text. Pure + side-effect-free.
func classifyStop(stopReason, errText string) stopDisposition {
	switch stopReason {
	case StopReasonUserBudget, StopReasonAgentBudget, StopReasonChannelBudget, StopReasonWorkspaceBudget:
		return stopAwaitBudget
	case StopReasonCircuitOpen, StopReasonRateLimited, StopReasonRunTimeout:
		return stopRetryTransient
	case StopReasonCanceled:
		// The run context was cancelled with no human stop on record (a stop is
		// settled before this point): this worker lost its lease, or the process
		// is shutting down. Both want the job back on the queue — a free retry —
		// not a "done" that would silently abandon unfinished work. The write is
		// lease-guarded, so a lost lease makes it a no-op.
		return stopRetryTransient
	case StopReasonStepLimit, StopReasonRunTokenLimit:
		return stopFinalizePartial
	}
	reason := strings.ToLower(errText)
	switch {
	case strings.Contains(reason, "token budget"):
		return stopAwaitBudget
	case strings.Contains(reason, "circuit") || strings.Contains(reason, "rate limit") || strings.Contains(reason, "run time limit"):
		return stopRetryTransient
	default:
		return stopFinalizePartial
	}
}

// handleStoppedAgentTask classifies a RunStopped outcome into the right durable
// transition: a token-budget exhaustion pause (awaiting, resumes later, no
// retry-budget cost), a transient throttle (free retry), or a
// bounded-completion stop (step / per-run token limit — re-running won't help,
// so finalize with the partial progress). Classification is driven by the
// runner's stable StopReason CODE via classifyStop, not ad-hoc text matching,
// so the state machine can never be silently broken by a message wording change.
func handleStoppedAgentTask(ctx context.Context, agent *model.AiAgent, t *model.AgentTask, lease uuid.UUID, outcome *RunOutcome, runID *uuid.UUID, postStatus, notify func(string)) {
	switch classifyStop(outcome.StopReason, outcome.Error) {
	case stopAwaitBudget:
		// Daily AI budget hit: pause and resume after it resets. Tell the team
		// once so a quiet job isn't mistaken for a stuck one.
		if t.Attempt <= 1 {
			postStatus("I've paused on this — today's AI usage limit was reached. I'll pick it back up once it resets, or an admin can raise it in AI settings.")
			notify("Paused — today's AI usage limit was reached. I'll resume once it resets.")
		}
		commitAgentTaskState(ctx, t.Id, "await (daily AI budget)", func(c context.Context) error {
			return model.AwaitAgentTask(c, t.Id, lease, time.Hour, outcome.Error, runID)
		})

	case stopRetryTransient:
		// A genuinely transient stall clears in seconds. But a persistently
		// unavailable model (circuit stuck open, every run timing out) would
		// otherwise take a "free" retry every 90s FOREVER — the job shows
		// "running" indefinitely and never resolves, which reads as broken and
		// is the "why is it continuously running" complaint. Bound the free
		// retries by wall-clock age: past the window, treat the outage as a real
		// failure so the job reaches a calm terminal state with an honest note,
		// instead of churning silently.
		if time.Since(t.CreatedAt) > maxTransientRetryWindow {
			body := "The AI service has been unavailable for a while, so I couldn't finish this. Re-assign it to me once things recover and I'll pick it back up."
			postStatus(body)
			notify(body)
			commitAgentTaskState(ctx, t.Id, "finish (AI unavailable too long)", func(c context.Context) error {
				return model.FinishAgentTask(c, t.Id, lease, model.TaskFailed, "", helpers.FirstNonBlank(outcome.Error, "AI service unavailable"), runID)
			})
			return
		}
		// Transient infrastructure stall: retry soon, don't burn the budget.
		commitAgentTaskState(ctx, t.Id, "retry (transient stall)", func(c context.Context) error {
			return model.RetryAgentTask(c, t.Id, lease, 90*time.Second, false, outcome.Error, runID)
		})

	default: // stopFinalizePartial
		if continueInNewSession(ctx, t, lease, outcome, runID) {
			return
		}
		finalizeBoundedCompletion(ctx, t, lease, outcome, runID, postStatus, notify)
	}
}

// maxAgentTaskSessions bounds how many work sessions one job may use. With the
// agent's per-run step limit it is the most work one hand-off can take before
// a person is asked; the daily token budget bounds it as well.
const maxAgentTaskSessions = 3

// sessionContinueNote is the turn a job's next session starts with.
const sessionContinueNote = "You reached the step limit for one work session and are continuing in a new one. " +
	"Pick up where you left off: the results of your earlier calls are above, so do not repeat them. " +
	"Finish the task, or say plainly what is blocking it."

// continueSession is the durable transition, a seam for tests.
var continueSession = model.ContinueAgentTaskSession

// shouldContinueSession reports whether a run that stopped at a per-run bound
// should carry on in a new session: it stopped at a bound (not a budget, a
// person or an error), and it made progress, calling something it had not
// called before. A session that only repeated itself stops, since another would
// too. Pure.
func shouldContinueSession(outcome *RunOutcome) bool {
	if outcome == nil || outcome.FreshCalls == 0 {
		return false
	}
	return outcome.StopReason == StopReasonStepLimit || outcome.StopReason == StopReasonRunTokenLimit
}

// continueInNewSession re-queues the job to carry on from its saved
// conversation, quietly: the live status already shows it working, and the
// result is posted once, when the work ends. Returns false when the job may not
// continue (no progress, no saved conversation, or out of sessions), and the
// caller finalizes it as before.
func continueInNewSession(ctx context.Context, t *model.AgentTask, lease uuid.UUID, outcome *RunOutcome, runID *uuid.UUID) bool {
	if !shouldContinueSession(outcome) {
		return false
	}
	ok, err := continueSession(ctx, t.Id, lease, sessionContinueNote, maxAgentTaskSessions, runID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: continue job %s in a new session failed: %v", t.Id, err)
		return false
	}
	if ok {
		helpers.LogInfoWithContext(ctx, "agentTaskWorker: job %s continues in a new session (%d new calls last session)", t.Id, outcome.FreshCalls)
		WakeAgentTaskWorker()
	}
	return ok
}

// finalizeBoundedCompletion closes a job that stopped because it hit a hard
// completion bound (step / per-run token limit): re-running would hit the same
// wall, so it posts whatever progress it made and marks the job done.
func finalizeBoundedCompletion(ctx context.Context, t *model.AgentTask, lease uuid.UUID, outcome *RunOutcome, runID *uuid.UUID, postStatus, notify func(string)) {
	body := strings.TrimSpace(outcome.Result)
	if body == "" {
		body = "I made some progress but ran out of steps before finishing. Reply here and I'll keep going."
	} else {
		body += "\n\n(I ran out of steps here. Reply here if you want me to keep going.)"
	}
	postStatus(body)
	notify(body)
	commitAgentTaskState(ctx, t.Id, "finish (bounded completion)", func(c context.Context) error {
		return model.FinishAgentTask(c, t.Id, lease, model.TaskDone, body, outcome.Error, runID)
	})
}

// runDidWrite reports whether the run successfully executed at least one
// state-changing (non read-only) tool. Such a run must be finalized rather than
// retried (re-running would duplicate the write). Unknown/MCP tools are treated
// as writes by ToolIsReadOnly (fail-safe), so an external action always counts.
func runDidWrite(outcome *RunOutcome) bool {
	for _, name := range outcome.ToolsSucceeded {
		if !ai.ToolIsReadOnly(name) {
			return true
		}
	}
	return false
}

// finalizeCompletedWork closes a job whose write(s) already landed but which did
// not reach a clean RunSucceeded (e.g. the final summary call hit a transient
// fault). It posts a ground-truth result built from what actually executed (so
// the live "working…" comment advances to a real outcome instead of stranding),
// and marks the job done so it is never retried into a duplicate write.
func finalizeCompletedWork(ctx context.Context, t *model.AgentTask, lease uuid.UUID, outcome *RunOutcome, runID *uuid.UUID, postStatus, notify func(string)) {
	body := strings.TrimSpace(outcome.Result)
	if body == "" {
		body = landedWorkFallback(outcome.ToolsSucceeded)
	}
	if tf := toolsUsedFooter(outcome.ToolsSucceeded); tf != "" {
		body += "\n\n" + tf
	}
	if nf := failedToolsNote(outcome.FailedTools); nf != "" {
		body += "\n\n" + nf
	}
	postStatus(body)
	notify(body)
	commitAgentTaskState(ctx, t.Id, "finish (work already landed)", func(c context.Context) error {
		return model.FinishAgentTask(c, t.Id, lease, model.TaskDone, body, "", runID)
	})
}

// landedWorkFallback is what a run says when its own summary is missing. Only
// writes that finished count as done: a tool that merely started background
// work (code_pr) has not completed anything, and on the demo a failed coding
// run was announced as "Done — completed the requested changes" this way.
func landedWorkFallback(succeeded []string) string {
	for _, name := range succeeded {
		if !ai.ToolIsReadOnly(name) && !ai.ToolDefersResult(name) {
			return "Done — completed the requested changes."
		}
	}
	return "Started — I'll post the result here when it's ready."
}

// agentTaskRetryBackoff is the delay before a penalized retry of a job: roughly
// linear in the attempt already made, floored and capped so a genuine fault is
// retried soon but a repeatedly failing job backs off. Pure, so every retry path
// (a failed run, an abandoned run) schedules the same way.
func agentTaskRetryBackoff(t *model.AgentTask) time.Duration {
	backoff := time.Duration(t.Attempt) * 30 * time.Second
	if backoff < 30*time.Second {
		backoff = 30 * time.Second
	}
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	return backoff
}

// scheduleAgentTaskRetry re-queues a failed job with exponential-ish backoff
// (penalizing the retry budget), so a genuine fault is retried a bounded number
// of times.
func scheduleAgentTaskRetry(ctx context.Context, t *model.AgentTask, lease uuid.UUID, errMsg string, runID *uuid.UUID) {
	commitAgentTaskState(ctx, t.Id, "retry (run fault)", func(c context.Context) error {
		return model.RetryAgentTask(c, t.Id, lease, agentTaskRetryBackoff(t), true, errMsg, runID)
	})
}

// taskStatusPoster maintains ONE evolving status comment for an agent task run:
// it posts a placeholder ("On it…"), updates it live as stages complete
// ("Working… using: …"), and edits it to the final result — the Notion/Slack
// "watch it work" lifecycle in the task's own thread, as the agent's badged
// principal. Resolving the bot principal + task once keeps per-step edits cheap.
// All edits are loop-safe (workflow-generated context) and best-effort: a
// posting failure never blocks the job's state machine.
type taskStatusPoster struct {
	agent       *model.AiAgent
	taskUUID    uuid.UUID
	projectUUID string
	botInfo     *userModels.UserInfo
	dgraphTask  *dgraphStruct.DgraphTask

	mu           sync.Mutex
	commentUUID  uuid.UUID // zero until the comment is created
	lastProgress time.Time
}

// taskProgressThrottle bounds how often the live "working…" line is edited, so
// a fast multi-step run doesn't churn the comment thread / MQTT.
const taskProgressThrottle = 3 * time.Second

// newTaskStatusPoster resolves the agent's principal + the task once. Returns
// nil when either can't be resolved (the caller then falls back to best-effort
// one-shot posts), so a resolution miss never blocks the run.
func newTaskStatusPoster(ctx context.Context, agent *model.AiAgent, t *model.AgentTask) *taskStatusPoster {
	taskUUID, err := assignmentTaskUUID(t)
	if err != nil {
		return nil
	}
	bot, berr := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey))
	if berr != nil || bot == nil || bot.UUID == "" {
		return nil
	}
	botInfo, ierr := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if ierr != nil || botInfo == nil {
		return nil
	}
	dgraphTask, terr := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUID.String(), botInfo.UserDgraphInfo.Uid)
	if terr != nil || dgraphTask == nil || dgraphTask.Project == nil {
		return nil
	}
	return &taskStatusPoster{
		agent:       agent,
		taskUUID:    taskUUID,
		projectUUID: dgraphTask.Project.Uuid,
		botInfo:     botInfo,
		dgraphTask:  dgraphTask,
	}
}

// Set posts the status comment (first call) or edits it to text (forced; used
// for the "On it" ack and the final result). Loop-safe + best-effort.
func (p *taskStatusPoster) Set(ctx context.Context, text string) {
	if p == nil || strings.TrimSpace(text) == "" {
		return
	}
	ctx = helpers.WithWorkflowGenerated(ctx)
	html := commentTextToHTML(text)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil {
		input := &taskAdapter.CreateOrUpdateTaskCommentInput{CommentBody: html, TaskUuid: p.taskUUID.String(), SkipGitHubSync: true}
		res, cerr := taskBusiness.CreateTaskComment(ctx, p.taskUUID, p.dgraphTask, p.botInfo, input, nil)
		if cerr != nil || res == nil {
			helpers.LogErrorWithContext(ctx, "agentTaskWorker: create status comment failed (task=%s): %v", p.taskUUID, cerr)
			return
		}
		if id, perr := uuid.Parse(res.Uuid); perr == nil {
			p.commentUUID = id
		}
		return
	}
	p.editLocked(ctx, html)
}

// moveDelegatedTask changes a delegated task's status as the agent. A seam, so
// MarkStarted is tested without the graph.
var moveDelegatedTask = func(ctx context.Context, p *taskStatusPoster, status string) error {
	return taskBusiness.UpdateTaskStatusByTaskUUID(ctx, p.taskUUID, status, p.dgraphTask, &p.botInfo.UserDgraphInfo)
}

// shouldMarkStarted reports whether a task in this category has not been
// started yet, so an agent starting on it should move it to in progress. A task
// someone already moved on (in progress, in review, done) is left alone. Pure.
func shouldMarkStarted(category string) bool {
	return category == dgraphStruct.TASK_STATUS_TODO || category == dgraphStruct.TASK_STATUS_BACKLOG
}

// MarkStarted moves the delegated task to in progress when the agent starts
// working on it and it has not been started. It is tagged workflow-generated,
// like every agent write, so the move cannot set off other agents or
// workflows. Nil-safe and best-effort.
func (p *taskStatusPoster) MarkStarted(ctx context.Context) {
	if p == nil || p.dgraphTask == nil || !shouldMarkStarted(p.dgraphTask.Status) {
		return
	}
	if err := moveDelegatedTask(helpers.WithWorkflowGenerated(ctx), p, dgraphStruct.TASK_STATUS_INPROGRESS); err != nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: mark delegated task started failed (task=%s): %v", p.taskUUID, err)
		return
	}
	p.dgraphTask.Status = dgraphStruct.TASK_STATUS_INPROGRESS
}

// Progress edits the status comment to a throttled "working…" line reflecting
// the tools used so far. No-op until the comment exists (so a retry that never
// posted an ack stays quiet).
func (p *taskStatusPoster) Progress(ctx context.Context, tools []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commentUUID == uuid.Nil {
		return
	}
	if time.Since(p.lastProgress) < taskProgressThrottle {
		return
	}
	line := "Working on it…"
	if tf := toolsUsedFooter(tools); tf != "" {
		line += "\n\n" + tf
	}
	p.lastProgress = time.Now()
	p.editLocked(helpers.WithWorkflowGenerated(ctx), commentTextToHTML(line))
}

// editLocked edits the status comment in place. Caller holds p.mu. The minimal
// raw-comment stub carries only the project uuid UpdateTaskCommentBody needs for
// its MQTT publish; GitHub sync is skipped (the agent's status is not a GitHub
// comment).
func (p *taskStatusPoster) editLocked(ctx context.Context, html string) {
	input := &taskAdapter.CreateOrUpdateTaskCommentInput{
		Uuid:           p.commentUUID.String(),
		CommentBody:    html,
		TaskUuid:       p.taskUUID.String(),
		SkipGitHubSync: true,
	}
	raw := &dgraphStruct.DgraphComment{
		Task: &dgraphStruct.DgraphTask{Uuid: p.taskUUID.String(), Project: &dgraphStruct.DgraphProject{Uuid: p.projectUUID}},
	}
	if uerr := taskBusiness.UpdateTaskCommentBody(ctx, p.commentUUID, input, raw, nil); uerr != nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: edit status comment failed (task=%s): %v", p.taskUUID, uerr)
	}
}

// postAgentTaskStatus posts text as a comment on the assigned task, authored by
// the agent's own badged bot principal, so progress reads as the AI teammate
// working in the task's own comment thread. Best-effort: a posting failure is
// logged but never blocks the job's state machine (the run already happened).
func postAgentTaskStatus(ctx context.Context, agent *model.AiAgent, t *model.AgentTask, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	taskUUID, err := assignmentTaskUUID(t)
	if err != nil {
		return
	}
	// Tag the comment write as workflow-generated so the agent's own status
	// comment never re-triggers the comment-resume listener (loop-safe).
	ctx = helpers.WithWorkflowGenerated(ctx)
	bot, berr := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey))
	if berr != nil || bot == nil || bot.UUID == "" {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: resolve agent principal failed (agent=%s): %v", agent.Id, berr)
		return
	}
	botInfo, ierr := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if ierr != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: build bot user info failed (agent=%s): %v", agent.Id, ierr)
		return
	}
	dgraphTask, terr := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUID.String(), botInfo.UserDgraphInfo.Uid)
	if terr != nil || dgraphTask == nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: get task for comment failed (task=%s): %v", taskUUID, terr)
		return
	}
	input := &taskAdapter.CreateOrUpdateTaskCommentInput{
		CommentBody: commentTextToHTML(text),
		TaskUuid:    taskUUID.String(),
	}
	if _, cerr := taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTask, botInfo, input, nil); cerr != nil {
		helpers.LogErrorWithContext(ctx, "agentTaskWorker: post status comment failed (task=%s): %v", taskUUID, cerr)
	}
}

// commentTextToHTML converts a plain-text agent status into the minimal,
// safe HTML the comment store/render path expects: each line HTML-escaped and
// wrapped in a paragraph, blank lines preserved as spacing. No model-authored
// markup is trusted (everything is escaped), so a status comment can never
// inject markup into the task thread.
func commentTextToHTML(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var b strings.Builder
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			b.WriteString("<p></p>")
			continue
		}
		b.WriteString("<p>")
		b.WriteString(html.EscapeString(ln))
		b.WriteString("</p>")
	}
	return b.String()
}

// alreadyRan reports whether a run has ever executed for this job. RunAgent
// records its run id on the task (last_run_id) via the very first CreateRun, and
// every requeue path (retry / await / crash-reclaim) preserves it, so this is a
// durable "not the first execution" signal that — unlike the attempt counter —
// survives the deliberate attempt rollback on a transient stall. Used to post
// the in-thread acknowledgement exactly once.
func alreadyRan(t *model.AgentTask) bool {
	return t.LastRunId != nil
}

// codePRAckMarker is the durable one-time-ack stamp for a code_pr task. code_pr
// never uses the durable conversation (that machinery is for the tool-loop
// path), so we reuse the messages column as a "has been dispatched" flag: it
// survives a needs-human pause+resume (ResumeAgentTaskWithFollowup only appends)
// and a crash reclaim (which preserves messages), so the "On it" note is posted
// exactly once. A valid one-element JSON array so the resume-path concat and the
// jsonb column stay well-formed.
const codePRAckMarker = `[{"role":"system","content":"code_pr:dispatched"}]`

// codePRAlreadyAcked reports whether a code_pr task has already been dispatched
// (and thus acknowledged) at least once, via the durable marker above.
func codePRAlreadyAcked(t *model.AgentTask) bool {
	m := strings.TrimSpace(t.Messages)
	return m != "" && m != "[]"
}

// firstNonEmpty returns the first non-blank string.

// toolsUsedFooter renders a compact, human-readable disclosure of the tools an
// agent actually used in a run ("Worked with: search workspace, list tasks."),
// so a reply/status is transparent about HOW it got the answer (the "show which
// sources/tools it used" half of the in-thread progress lifecycle). Names are
// humanized (underscores to spaces, mcp_ prefix dropped), de-duplicated, and
// capped so the line stays short. Returns "" when no tools ran (a purely
// conversational turn discloses nothing).
func toolsUsedFooter(tools []string) string {
	const maxShown = 8
	seen := map[string]bool{}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		label := humanizeToolName(t)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	if len(out) == 0 {
		return ""
	}
	rest := 0
	if len(out) > maxShown {
		rest = len(out) - maxShown
		out = out[:maxShown]
	}
	footer := "Worked with: " + strings.Join(out, ", ")
	if rest > 0 {
		footer += ", and " + strconv.Itoa(rest) + " more"
	}
	return footer + "."
}

// humanizeToolName turns a registry/MCP tool id into a readable label:
// "search_workspace" -> "search workspace", "mcp_github_list_prs" -> "github list prs".
func humanizeToolName(name string) string {
	name = strings.TrimPrefix(name, "mcp_")
	name = strings.ReplaceAll(name, "_", " ")
	return strings.TrimSpace(name)
}

// failedToolsNote renders an honest disclosure of tools whose calls ERRORED
// during a run, so a confident-but-false "done" summary (common on weaker
// models that narrate success after a failed write) cannot silently hide a
// no-op. Names are humanized, de-duplicated, and capped. Returns "" when no
// tool errored.
func failedToolsNote(tools []string) string {
	const maxShown = 8
	seen := map[string]bool{}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		label := humanizeToolName(t)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	if len(out) == 0 {
		return ""
	}
	rest := 0
	if len(out) > maxShown {
		rest = len(out) - maxShown
		out = out[:maxShown]
	}
	note := "Heads up: some actions did not complete and may need a retry or a permissions check: " + strings.Join(out, ", ")
	if rest > 0 {
		note += ", and " + strconv.Itoa(rest) + " more"
	}
	return note + "."
}
