package models

// Durable agent-task queue (migration 111). An agent RUN is a single bounded
// in-process tool-loop (ai_agent_runs); an agent TASK is the durable JOB that
// drives one piece of handed-off work (today: a project task assigned to an
// agent) to a terminal state across restarts, retries, and worker crashes. The
// worker leases a runnable row, executes it through the runner, and records the
// outcome here. This package only persists/reads tasks; the runner executes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Agent-task state constants (aligned with the migration 111 CHECK).
const (
	TaskQueued   = "queued"
	TaskRunning  = "running"
	TaskAwaiting = "awaiting_input"
	TaskDone     = "done"
	TaskFailed   = "failed"
	// TaskCancelled is the terminal state of a job a human stopped (migration
	// 134). Deliberately NOT 'done' (the work wasn't finished) and not 'failed'
	// (nothing went wrong), so reliability rollups stay meaningful.
	TaskCancelled = "cancelled"
)

// OpenTaskStates are the non-terminal states: a job in one of these is still
// somebody's problem (lined up, running, or waiting on a human). Kept in one
// place so every "is this job still open" check agrees.
var OpenTaskStates = []string{TaskQueued, TaskRunning, TaskAwaiting}

// Agent-task source kinds. Open set; v1 ships task assignment.
const (
	TaskSourceAssignment = "task_assignment"
)

// AgentTask mirrors a row of the ai_agent_tasks table.
type AgentTask struct {
	Id          uuid.UUID  `json:"id"`
	AgentId     uuid.UUID  `json:"agent_id"`
	SourceType  string     `json:"source_type"`
	SourceId    string     `json:"source_id"`
	Prompt      string     `json:"prompt"`
	RunAsUserId *uuid.UUID `json:"run_as_user_id,omitempty"`
	// TriggeredBy is the person who asked (the @mention/message author), used to
	// notify the RIGHT human when the run blocks or finishes. Distinct from
	// RunAsUserId (the owner whose permissions the run uses). Nil for
	// schedule/event/task-assignment jobs.
	TriggeredBy *uuid.UUID `json:"triggered_by,omitempty"`
	// DelegationHop / DelegationChain persist a delegation chain's lineage across
	// the process boundary a durable job crosses (enqueued by one goroutine,
	// executed later, possibly on another node), so a durable hop can detect a
	// cycle by IDENTITY instead of relying on the hop budget alone. Zero/empty is
	// the "not part of a chain" reading, which is what a human-triggered job is.
	DelegationHop   int        `json:"delegation_hop"`
	DelegationChain []string   `json:"delegation_chain,omitempty"`
	State           string     `json:"state"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	NextAttemptAt   time.Time  `json:"next_attempt_at"`
	LeaseToken      *uuid.UUID `json:"-"`
	LeaseExpires    *time.Time `json:"-"`
	LastRunId       *uuid.UUID `json:"last_run_id,omitempty"`
	LastError       *string    `json:"last_error,omitempty"`
	Result          *string    `json:"result,omitempty"`
	Messages        string     `json:"-"` // raw JSON array of the durable conversation
	// Surface is the raw JSON reply-surface descriptor (see business/AIAgent
	// Surface): where a non-task job posts/edits its status. Empty/absent =>
	// the legacy task surface, so existing jobs are unchanged.
	Surface   string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// CancelRequestedAt / CancelRequestedBy record a human's request to stop the
	// job (migration 134). Cancellation is cooperative: the worker that holds the
	// lease observes the request, unwinds its in-flight run, and writes the
	// terminal 'cancelled' row — so there is still exactly one writer per lease.
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	CancelRequestedBy *uuid.UUID `json:"cancel_requested_by,omitempty"`
	// Sessions is how many work sessions the job has used (migration 169).
	Sessions int `json:"sessions"`
}

const agentTaskColumns = `id, agent_id, source_type, source_id, prompt, run_as_user_id,
	state, attempt, max_attempts, next_attempt_at, lease_token, lease_expires_at,
	last_run_id, last_error, result, messages, surface, triggered_by, created_at, updated_at, ended_at,
	cancel_requested_at, cancel_requested_by, delegation_hop, delegation_chain, sessions`

func scanAgentTask(s scanner) (*AgentTask, error) {
	var t AgentTask
	var runAs, leaseTok, lastRun, triggeredBy, cancelBy uuid.NullUUID
	var leaseExp, endedAt, cancelAt sql.NullTime
	var lastErr, result, surface sql.NullString
	// delegation_chain is jsonb, so it arrives as bytes and is decoded below.
	var delegationChain []byte
	// EVERY COLUMN IN agentTaskColumns MUST BE SCANNED HERE, IN ORDER.
	//
	// Migration 137 added delegation_hop and delegation_chain, and agentTaskColumns was
	// extended, but these two destinations were not. Nothing failed at build time — the
	// mismatch only exists at runtime, where database/sql reported "expected 25 destination
	// arguments in Scan, not 23" and every ClaimNextRunnable returned that error. The worker
	// retried every five seconds and claimed nothing, so no durable agent task ran at all
	// while the service reported itself healthy. helpers.TestSharedColumnListsMatchTheirScanDestinations
	// now counts both lists across every model using this pattern, so the next added column
	// cannot repeat it.
	err := s.Scan(
		&t.Id, &t.AgentId, &t.SourceType, &t.SourceId, &t.Prompt, &runAs,
		&t.State, &t.Attempt, &t.MaxAttempts, &t.NextAttemptAt, &leaseTok, &leaseExp,
		&lastRun, &lastErr, &result, &t.Messages, &surface, &triggeredBy, &t.CreatedAt, &t.UpdatedAt, &endedAt,
		&cancelAt, &cancelBy, &t.DelegationHop, &delegationChain, &t.Sessions,
	)
	if err != nil {
		return nil, err
	}
	// The column is NOT NULL with a CHECK that it is an array, and the writer stores []
	// rather than NULL, so a decode failure means the row is malformed rather than absent —
	// worth surfacing instead of silently yielding an empty chain, because an empty chain
	// reads as "not part of a delegation walk" and would defeat cycle detection by identity.
	if len(delegationChain) > 0 {
		if err := json.Unmarshal(delegationChain, &t.DelegationChain); err != nil {
			return nil, fmt.Errorf("decode delegation_chain for agent task %s: %w", t.Id, err)
		}
	}
	if cancelAt.Valid {
		t.CancelRequestedAt = &cancelAt.Time
	}
	if cancelBy.Valid {
		id := cancelBy.UUID
		t.CancelRequestedBy = &id
	}
	if surface.Valid {
		t.Surface = surface.String
	}
	if triggeredBy.Valid {
		id := triggeredBy.UUID
		t.TriggeredBy = &id
	}
	if runAs.Valid {
		id := runAs.UUID
		t.RunAsUserId = &id
	}
	if leaseTok.Valid {
		id := leaseTok.UUID
		t.LeaseToken = &id
	}
	if leaseExp.Valid {
		t.LeaseExpires = &leaseExp.Time
	}
	if lastRun.Valid {
		id := lastRun.UUID
		t.LastRunId = &id
	}
	if lastErr.Valid {
		t.LastError = &lastErr.String
	}
	if result.Valid {
		t.Result = &result.String
	}
	if endedAt.Valid {
		t.EndedAt = &endedAt.Time
	}
	return &t, nil
}

// EnqueueAgentTask inserts a new queued job, or returns the existing open job's
// id when one already exists for the same (source_type, source_id, agent_id)
// — so re-assigning a task or a duplicate event never stacks duplicate work.
// Returns (id, created): created=false means an open job already existed.
func EnqueueAgentTask(ctx context.Context, t *AgentTask) (uuid.UUID, bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 3
	}
	if strings.TrimSpace(t.SourceType) == "" {
		t.SourceType = TaskSourceAssignment
	}
	id := uuid.New()
	// The chain is stored as jsonb; an empty chain is written as [] rather than
	// NULL so the column's array invariant holds for every row and readers never
	// have to distinguish "no chain" from "not set".
	chainArg := []byte("[]")
	if len(t.DelegationChain) > 0 {
		if encoded, merr := json.Marshal(t.DelegationChain); merr == nil {
			chainArg = encoded
		}
	}
	var surfaceArg interface{}
	if strings.TrimSpace(t.Surface) != "" {
		surfaceArg = t.Surface
	}
	const q = `INSERT INTO ai_agent_tasks
		(id, agent_id, source_type, source_id, prompt, run_as_user_id, state, max_attempts, next_attempt_at, surface, triggered_by, delegation_hop, delegation_chain)
		VALUES ($1,$2,$3,$4,$5,$6,'queued',$7, now(), $8, $9, $10, $11)
		ON CONFLICT (source_type, source_id, agent_id)
		WHERE state IN ('queued','running','awaiting_input')
		DO NOTHING
		RETURNING id`
	var newID uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q,
		id, t.AgentId, t.SourceType, t.SourceId, t.Prompt, t.RunAsUserId, t.MaxAttempts, surfaceArg, t.TriggeredBy, t.DelegationHop, chainArg).Scan(&newID)
	if errors.Is(err, sql.ErrNoRows) {
		// An open job already exists; return its id (not created).
		var existing uuid.UUID
		const sel = `SELECT id FROM ai_agent_tasks
			WHERE source_type=$1 AND source_id=$2 AND agent_id=$3
			  AND state IN ('queued','running','awaiting_input')
			ORDER BY created_at DESC LIMIT 1`
		if serr := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, sel, t.SourceType, t.SourceId, t.AgentId).Scan(&existing); serr != nil {
			return uuid.Nil, false, serr
		}
		return existing, false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/EnqueueAgentTask Failed err: %+v", err)
		return uuid.Nil, false, err
	}
	return newID, true, nil
}

// ClaimNextRunnable atomically leases the oldest due, runnable job (queued or
// awaiting_input whose next_attempt_at has passed), transitioning it to
// running with a fresh lease, and bumps attempt. Uses FOR UPDATE SKIP LOCKED so
// multiple workers/replicas never claim the same job. Returns (nil, nil) when
// nothing is due. leaseTTL is how long the claim is valid before the job is
// reclaimable as crashed.
//
// perAgentCap bounds how many tasks ONE agent may run at once: a candidate is
// skipped while its agent already has that many running. This lets a single AI
// teammate work several tasks in parallel (like a multitasking teammate) while
// staying fair — one busy agent never starves the others, and the global
// worker concurrency still caps total in-flight work. A cap <= 0 disables the
// per-agent limit (only the global cap applies).
func ClaimNextRunnable(ctx context.Context, leaseTTL time.Duration, perAgentCap int) (*AgentTask, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// The per-agent in-flight guard: skip a candidate whose agent is already at
	// its concurrency cap, so work spreads across teammates fairly. Evaluated
	// inside the claim transaction against committed 'running' rows.
	agentCapClause := ""
	args := []interface{}{}
	if perAgentCap > 0 {
		agentCapClause = ` AND (SELECT count(*) FROM ai_agent_tasks r
			WHERE r.agent_id = t.agent_id AND r.state='running') < $1`
		args = append(args, perAgentCap)
	}
	// A job with a pending stop is never started: the request normally settles a
	// queued/parked job outright, but a stop that lands in the same instant as a
	// claim would otherwise start work a person just cancelled.
	sel := `SELECT t.id FROM ai_agent_tasks t
		WHERE t.state IN ('queued','awaiting_input') AND t.next_attempt_at <= now()
		  AND t.cancel_requested_at IS NULL` +
		agentCapClause + `
		ORDER BY t.next_attempt_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1`
	var id uuid.UUID
	if err := tx.QueryRowContext(dbctx, sel, args...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	leaseToken := uuid.New()
	const upd = `UPDATE ai_agent_tasks
		SET state='running', attempt=attempt+1, lease_token=$2,
		    lease_expires_at=now() + $3::interval, updated_at=now()
		WHERE id=$1
		RETURNING ` + agentTaskColumns
	row := tx.QueryRowContext(dbctx, upd, id, leaseToken, fmt.Sprintf("%d seconds", int(leaseTTL.Seconds())))
	t, scanErr := scanAgentTask(row)
	if scanErr != nil {
		return nil, scanErr
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}

// RenewAgentTaskLease extends the lease of a job the CALLER still owns, so a
// long-but-live run is never reclaimed out from under itself while it is still
// executing. Guarded on the task id AND the lease token AND state='running', so
// only the current owner can extend: a worker whose lease already expired (and
// whose job was reclaimed by someone else) can never resurrect its claim, and a
// terminal/parked job is never re-armed.
//
// Returns held=false when no row matched — the lease is GONE (reclaimed,
// finished, or parked elsewhere). That is not a database fault: it is proof this
// worker no longer owns the job, and the caller must stop executing and stop
// writing rather than racing the new owner. A non-nil error is a transient DB
// fault (the caller may keep running and retry the heartbeat).
//
// cancelRequested reports, in the SAME round trip, whether a human has asked for
// this job to stop. The heartbeat is already the one thing polling on a live run,
// so folding the flag into its RETURNING clause gives cooperative cancellation
// with no extra query and no extra timer.
//
// Does not touch updated_at: a heartbeat is liveness, not a state move, so the
// "last moved" timestamp the active-work views show stays truthful.
func RenewAgentTaskLease(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID, leaseTTL time.Duration) (held bool, cancelRequested bool, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	secs := int(leaseTTL.Seconds())
	if secs < 1 {
		secs = 1
	}
	const q = `UPDATE ai_agent_tasks
		SET lease_expires_at = now() + $3::interval
		WHERE id=$1 AND lease_token=$2 AND state='running'
		RETURNING cancel_requested_at IS NOT NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, leaseToken, fmt.Sprintf("%d seconds", secs))
	if err := row.Scan(&cancelRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		helpers.LogErrorWithContext(ctx, "models/RenewAgentTaskLease err: %+v", err)
		return false, false, err
	}
	return true, cancelRequested, nil
}

// AgentTaskLeaseHeld reports whether the caller STILL owns a job's lease (the row
// is 'running' with this exact lease token). Used to decide, after a dispatch
// path returned or panicked, whether the job was ever settled: if the lease is
// still held, no terminal transition landed and the caller must record one
// itself instead of leaving the job "working" until the lease expires. A read,
// so it never mutates state; an error is transient and the caller stays quiet.
func AgentTaskLeaseHeld(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT EXISTS(SELECT 1 FROM ai_agent_tasks
		WHERE id=$1 AND lease_token=$2 AND state='running')`
	var held bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, leaseToken).Scan(&held); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AgentTaskLeaseHeld err: %+v", err)
		return false, err
	}
	return held, nil
}

// ReclaimExpiredLeases re-queues jobs stuck 'running' past their lease (a
// worker crashed mid-run), so they are retried instead of stranded. Honors
// max_attempts: a job that has exhausted its attempts is failed with a note
// rather than re-queued forever. Returns the number reclaimed.
func ReclaimExpiredLeases(ctx context.Context) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// A job whose worker died while a human was stopping it must be honoured as
	// cancelled, not retried: re-queueing it would restart work somebody
	// explicitly asked to end, and the request would then be observed only by
	// the next worker (which would stop it again after burning a claim).
	const cancelQ = `UPDATE ai_agent_tasks
		SET state='cancelled', result=COALESCE(result,'Stopped — the worker running this ended before it could wrap up.'),
		    lease_token=NULL, lease_expires_at=NULL, ended_at=now(), updated_at=now()
		WHERE state='running' AND lease_expires_at < now() AND cancel_requested_at IS NOT NULL`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, cancelQ); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReclaimExpiredLeases cancel err: %+v", err)
		return 0, err
	}

	// Fail those that crashed and have no attempts left.
	const failQ = `UPDATE ai_agent_tasks
		SET state='failed', last_error='worker stopped mid-run and the retry budget was exhausted',
		    lease_token=NULL, lease_expires_at=NULL, ended_at=now(), updated_at=now()
		WHERE state='running' AND lease_expires_at < now() AND attempt >= max_attempts`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, failQ); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReclaimExpiredLeases fail err: %+v", err)
		return 0, err
	}

	// Re-queue the rest with a short delay so they aren't claimed instantly.
	const reQ = `UPDATE ai_agent_tasks
		SET state='queued', lease_token=NULL, lease_expires_at=NULL,
		    next_attempt_at=now() + interval '15 seconds', updated_at=now()
		WHERE state='running' AND lease_expires_at < now() AND attempt < max_attempts`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, reQ)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReclaimExpiredLeases requeue err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// FinishAgentTask marks a leased job terminal (done or failed) with its result
// and the run that produced it. Guarded on the lease token so a reclaimed job
// (taken over by another worker) isn't clobbered by the original worker.
func FinishAgentTask(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID, state, result, errMsg string, runID *uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var resultArg, errArg interface{}
	if strings.TrimSpace(result) != "" {
		resultArg = result
	}
	if strings.TrimSpace(errMsg) != "" {
		errArg = errMsg
	}
	const q = `UPDATE ai_agent_tasks
		SET state=$3, result=$4, last_error=$5, last_run_id=$6,
		    lease_token=NULL, lease_expires_at=NULL, ended_at=now(), updated_at=now()
		WHERE id=$1 AND lease_token=$2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, leaseToken, state, resultArg, errArg, runID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/FinishAgentTask err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RetryAgentTask returns a leased job to the queue with a backoff delay after a
// failure (so it is retried later). Guarded on the lease token. When penalize
// is false the attempt counter is rolled back, so a transient infrastructure
// stall (open circuit, rate limit, deadline) does not consume the job's failure
// retry budget — only a genuine fault does.
//
// On a penalized (genuine-fault) retry the saved conversation is also CLEARED,
// so the next attempt re-grounds from the clean prompt instead of replaying the
// approach that just failed. This prevents a doomed loop where a model anchors
// on its own earlier mistake (e.g. a wrong identifier) and re-sends an
// ever-growing, failing transcript every attempt — which both can never recover
// and needlessly burns the provider token budget. A NON-penalized (transient)
// retry keeps the conversation, since the work itself was fine and only the
// infrastructure stalled; crash-reclaim and awaiting_input resume likewise keep
// it (so a completed write is never repeated).
func RetryAgentTask(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID, backoff time.Duration, penalize bool, errMsg string, runID *uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var errArg interface{}
	if strings.TrimSpace(errMsg) != "" {
		errArg = errMsg
	}
	attemptExpr := "attempt"
	messagesExpr := "messages"
	if !penalize {
		attemptExpr = "GREATEST(attempt-1,0)"
	} else {
		// Genuine fault: discard the failed transcript so the retry starts cold.
		messagesExpr = "'[]'::jsonb"
	}
	q := `UPDATE ai_agent_tasks
		SET state='queued', attempt=` + attemptExpr + `, last_error=$4, last_run_id=$5,
		    messages=` + messagesExpr + `,
		    lease_token=NULL, lease_expires_at=NULL,
		    next_attempt_at=now() + $3::interval, updated_at=now()
		WHERE id=$1 AND lease_token=$2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, leaseToken, fmt.Sprintf("%d seconds", int(backoff.Seconds())), errArg, runID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RetryAgentTask err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AwaitAgentTask parks a leased job as awaiting_input (e.g. a token-budget pause
// or a needs-a-human blocker), with a delay before it becomes runnable again so
// a re-queue resumes it. The attempt counter is rolled back: waiting on a daily
// budget reset or a human is not a failed attempt and must not exhaust the
// retry budget. Guarded on the lease token.
func AwaitAgentTask(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID, retryAfter time.Duration, reason string, runID *uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var reasonArg interface{}
	if strings.TrimSpace(reason) != "" {
		reasonArg = reason
	}
	const q = `UPDATE ai_agent_tasks
		SET state='awaiting_input', attempt=GREATEST(attempt-1,0), last_error=$4, last_run_id=$5,
		    lease_token=NULL, lease_expires_at=NULL,
		    next_attempt_at=now() + $3::interval, updated_at=now()
		WHERE id=$1 AND lease_token=$2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, leaseToken, fmt.Sprintf("%d seconds", int(retryAfter.Seconds())), reasonArg, runID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AwaitAgentTask err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveAgentTaskMessages checkpoints the durable conversation for a leased job
// (called after each runner step), so a pause/crash can resume the SAME
// conversation. Lease-guarded so a reclaimed job taken over by another worker
// isn't clobbered.
//
// Like the other lease-guarded writes it reports a no-op as sql.ErrNoRows, so a
// caller can tell "the lease is gone, stop writing" apart from a transient DB
// fault worth retrying — a silently dropped checkpoint is what makes a whole
// job replay after a lease expiry.
func SaveAgentTaskMessages(ctx context.Context, id uuid.UUID, leaseToken uuid.UUID, messagesJSON string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(messagesJSON) == "" {
		messagesJSON = "[]"
	}
	const q = `UPDATE ai_agent_tasks SET messages=$3, updated_at=now()
		WHERE id=$1 AND lease_token=$2`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, leaseToken, messagesJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SaveAgentTaskMessages err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SeedAgentTaskMessages pre-populates the durable conversation of a FRESH,
// not-yet-claimed job (state='queued' with no saved messages), so a durable
// continuation handed off from a synchronous run resumes MID-conversation
// instead of restarting from the prompt. This is what makes a sync->durable
// hand-off safe: the runner rebuilds its dedupe set from the seeded tool calls,
// so a write the synchronous run already performed is never repeated. Guarded
// so it can only seed a queued job whose messages are still empty (it can never
// clobber a running/awaiting job or an already-seeded one). Returns true when a
// job was seeded.
func SeedAgentTaskMessages(ctx context.Context, id uuid.UUID, messagesJSON string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(messagesJSON) == "" || strings.TrimSpace(messagesJSON) == "[]" {
		return false, nil // nothing to seed
	}
	const q = `UPDATE ai_agent_tasks SET messages=$2, updated_at=now()
		WHERE id=$1 AND state='queued'
		  AND (messages IS NULL OR messages='[]'::jsonb OR jsonb_array_length(messages)=0)`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, messagesJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SeedAgentTaskMessages err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ContinueAgentTaskSession puts a leased job that reached its per-run bound
// back on the queue to carry on in a new session: the saved conversation is
// kept, note is appended as a user turn, and the attempt is not charged. It
// only succeeds while the job has a saved conversation to continue and has
// used fewer than maxSessions sessions, so continuing is always bounded.
// Returns false (no error) when the job may not continue.
func ContinueAgentTaskSession(ctx context.Context, id, leaseToken uuid.UUID, note string, maxSessions int, runID *uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	turn, err := json.Marshal([]map[string]string{{"role": "user", "content": strings.TrimSpace(note)}})
	if err != nil {
		return false, err
	}
	const q = `UPDATE ai_agent_tasks
		SET state='queued', sessions=sessions+1, attempt=GREATEST(attempt-1,0),
		    messages = messages || $4::jsonb,
		    last_error=NULL, last_run_id=$5,
		    lease_token=NULL, lease_expires_at=NULL,
		    next_attempt_at=now(), updated_at=now()
		WHERE id=$1 AND lease_token=$2 AND sessions < $3
		  AND jsonb_array_length(messages) > 0`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, leaseToken, maxSessions, string(turn), runID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ContinueAgentTaskSession err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResumeAgentTaskWithFollowup re-queues an awaiting_input job so a worker picks
// it up now. The human's follow-up is appended BOTH as a user turn to the
// durable conversation (so a step-replay resume continues with full context)
// AND to the prompt (the fallback used when there is no saved conversation).
// Only transitions from awaiting_input; other states are left alone. Returns
// true when a job was resumed.
func ResumeAgentTaskWithFollowup(ctx context.Context, id uuid.UUID, followup string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	add := strings.TrimSpace(followup)
	// The user turn appended to the saved conversation. json.Marshal escapes the
	// text safely; we wrap it as a one-element array for jsonb || concatenation.
	turn := "[]"
	if add != "" {
		if b, merr := json.Marshal([]map[string]string{{"role": "user", "content": "A teammate replied on the task: " + add}}); merr == nil {
			turn = string(b)
		}
	}
	const q = `UPDATE ai_agent_tasks
		SET state='queued',
		    messages = CASE WHEN jsonb_array_length(messages) > 0 THEN messages || $3::jsonb ELSE messages END,
		    prompt = CASE WHEN $2='' THEN prompt
		                  ELSE prompt || E'\n\nA teammate replied on the task: "' || $2 || E'"\nContinue the work, taking their reply into account.' END,
		    last_error=NULL, lease_token=NULL, lease_expires_at=NULL,
		    next_attempt_at=now(), updated_at=now()
		WHERE id=$1 AND state='awaiting_input'`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, add, turn)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ResumeAgentTaskWithFollowup err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentActiveTask is a durable job that is still open (queued / running /
// awaiting_input) joined to its agent's display fields, for the cross-agent
// "what are my teammates working on right now, and where are they stuck" view.
// It reuses the ai_agent_tasks record (the authoritative durable-job state), so
// there is no parallel store.
type AgentActiveTask struct {
	Id             uuid.UUID
	AgentId        uuid.UUID
	AgentName      string
	AgentAvatarKey *string
	SourceType     string
	SourceId       string
	Surface        string
	State          string
	Attempt        int
	LastError      *string
	NextAttemptAt  time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// StopRequested is true when a human has asked this job to stop and its
	// worker has not settled it yet, so the feed can show "stopping…" rather than
	// offering a Stop control that is already pressed.
	StopRequested bool
	// The job's people, carried so a reader can be authorized against ONE rule
	// (may this person stop this work?) wherever a job is listed, instead of each
	// feed inferring permission from how it happened to be scoped.
	AgentCreatedBy uuid.UUID
	TriggeredBy    *uuid.UUID
	RunAsUserId    *uuid.UUID
}

// ExecActiveTasks runs a pre-built open-durable-job query (see domain/AIAgent,
// which owns the WHERE/scoping/ordering) and scans the joined agent+task rows.
// The projected column order is the contract shared with the domain builders:
// id, agent_id, agent name, avatar_key, source_type, source_id, surface, state,
// attempt, last_error, next_attempt_at, created_at, updated_at, stop_requested,
// agent created_by, triggered_by, run_as_user_id. Every active-work view (owner
// scoped, actor scoped, entity scoped) shares this executor.
func ExecActiveTasks(ctx context.Context, query string, args []interface{}) ([]*AgentActiveTask, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecActiveTasks failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AgentActiveTask
	for rows.Next() {
		var it AgentActiveTask
		var avatarKey, surface, lastErr sql.NullString
		var triggeredBy, runAs uuid.NullUUID
		if scanErr := rows.Scan(&it.Id, &it.AgentId, &it.AgentName, &avatarKey, &it.SourceType,
			&it.SourceId, &surface, &it.State, &it.Attempt, &lastErr, &it.NextAttemptAt,
			&it.CreatedAt, &it.UpdatedAt, &it.StopRequested,
			&it.AgentCreatedBy, &triggeredBy, &runAs); scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ExecActiveTasks scan err: %+v", scanErr)
			return nil, scanErr
		}
		if triggeredBy.Valid {
			id := triggeredBy.UUID
			it.TriggeredBy = &id
		}
		if runAs.Valid {
			id := runAs.UUID
			it.RunAsUserId = &id
		}
		if avatarKey.Valid {
			it.AgentAvatarKey = &avatarKey.String
		}
		if surface.Valid {
			it.Surface = surface.String
		}
		if lastErr.Valid {
			it.LastError = &lastErr.String
		}
		out = append(out, &it)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// ListAgentTasksBySource returns the agent tasks for one source entity (e.g. a
// project task), newest first — for surfacing "what the AI teammate is doing".
func ListAgentTasksBySource(ctx context.Context, sourceType, sourceID string) ([]*AgentTask, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT ` + agentTaskColumns + ` FROM ai_agent_tasks
		WHERE source_type=$1 AND source_id=$2 ORDER BY created_at DESC LIMIT 100`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, sourceType, sourceID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListAgentTasksBySource err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AgentTask
	for rows.Next() {
		t, scanErr := scanAgentTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAgentTasksForEntity finds EVERY durable job that targets one surface
// entity (a channel post uuid, a chat message uuid, or a task uuid), across ALL
// source types, so a single follow-up handler can continue the right job no
// matter how it was enqueued:
//   - durable mention/thread jobs carry the id in the surface descriptor
//     (surface->>'post_id' or surface->>'message_id');
//   - task-assignment jobs use source_id = the task id;
//   - code_pr jobs use a prefixed source_id ("post:<id>" / "msg:<id>" /
//     "task:<id>", per codepr.SourceID) — passed in via sourceIDs so this model
//     stays free of that business convention.
//
// entityID matches the surface json; sourceIDs is the full set of source_id
// values that also denote this entity (the caller includes the bare id and any
// prefixed variants). Newest first, capped. Deduped by row id by the query.
func ListAgentTasksForEntity(ctx context.Context, entityID string, sourceIDs []string) ([]*AgentTask, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	entityID = strings.TrimSpace(entityID)
	if entityID == "" && len(sourceIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT ` + agentTaskColumns + ` FROM ai_agent_tasks
		WHERE ($1 <> '' AND (surface->>'post_id' = $1 OR surface->>'message_id' = $1))
		   OR source_id = ANY($2)
		ORDER BY created_at DESC LIMIT 100`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, entityID, pq.Array(sourceIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListAgentTasksForEntity err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AgentTask
	for rows.Next() {
		t, scanErr := scanAgentTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueueClaimSummary is what the queue looks like to something asking whether
// anybody is working it.
type QueueClaimSummary struct {
	// Overdue is how many runnable jobs have been due since before the cutoff
	// and are still unclaimed. Runnable means exactly what ClaimNextRunnable
	// means by it, so a job this counts is one a worker would have taken.
	Overdue int
	// OldestDue is when the longest-waiting of them became due.
	OldestDue *time.Time
	// LiveLeases is how many jobs are running under a lease that has not
	// expired: the number of workers provably busy right now.
	LiveLeases int
}

// SummariseUnclaimed answers "is anybody claiming from this queue", for the
// system check that asks it.
//
// The two numbers are read together because neither means anything alone. An
// overdue backlog with live leases is a busy worker behind on its work, which
// is a capacity question and not a fault. An overdue backlog with NO live lease
// is a queue nobody is draining: every replica is SERVICE_ROLE=api, or the
// worker is down, or AI was switched off with work still queued. That is the
// state that stays silent otherwise, because a job that is never claimed never
// fails and never posts.
func SummariseUnclaimed(ctx context.Context, dueBefore time.Time) (QueueClaimSummary, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var s QueueClaimSummary
	var oldest sql.NullTime
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `
		SELECT
		  (SELECT count(*) FROM ai_agent_tasks
		    WHERE state IN ('queued','awaiting_input') AND next_attempt_at <= $1
		      AND cancel_requested_at IS NULL),
		  (SELECT min(next_attempt_at) FROM ai_agent_tasks
		    WHERE state IN ('queued','awaiting_input') AND next_attempt_at <= $1
		      AND cancel_requested_at IS NULL),
		  (SELECT count(*) FROM ai_agent_tasks
		    WHERE state = 'running' AND lease_expires_at > now())`,
		dueBefore).Scan(&s.Overdue, &oldest, &s.LiveLeases)
	if err != nil {
		return QueueClaimSummary{}, err
	}
	if oldest.Valid {
		t := oldest.Time
		s.OldestDue = &t
	}
	return s, nil
}

// FirstAgentTaskForSource returns the id and prompt of the agent's first job
// for one source: the original request that later follow-ups on the same thread
// or task build on. found is false when there is none.
func FirstAgentTaskForSource(ctx context.Context, agentID uuid.UUID, sourceType, sourceID string) (id uuid.UUID, prompt string, found bool, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT id, prompt FROM ai_agent_tasks
		WHERE agent_id=$1 AND source_type=$2 AND source_id=$3
		ORDER BY created_at ASC LIMIT 1`
	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, agentID, sourceType, sourceID).Scan(&id, &prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/FirstAgentTaskForSource err: %+v", err)
		return uuid.Nil, "", false, err
	}
	return id, prompt, true, nil
}
