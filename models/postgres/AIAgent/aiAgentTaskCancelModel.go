package models

// Cooperative cancellation of a durable agent job (migration 134).
//
// A job could be started but never stopped: once enqueued it ran to a terminal
// state (or sat blocked) whether or not the person who asked still wanted it.
// "Stop" is table stakes for an agent that acts on real workspace data, and an
// operator needs to halt a misdirected run without waiting for a wall-clock
// limit or restarting a worker.
//
// Why a REQUEST and not a state flip: the worker holding the lease owns the
// in-flight run. If an API call flipped the row terminal, two writers would race
// over the same job (the API and the worker's own finalizer) and the transcript,
// result and lease bookkeeping could tear. So a stop is recorded here, the
// worker's lease heartbeat observes it, unwinds its run, and writes the one
// terminal row — the same single-writer discipline every other transition uses.
//
// A job that is NOT leased (queued / awaiting_input) has nothing in flight, so
// the request and the terminal transition happen in the same statement.

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Outcomes of a cancellation request, so a caller can tell the person what
// actually happened rather than a generic "ok".
const (
	// CancelStopped: the job was not running, so it is already terminal.
	CancelStopped = "stopped"
	// CancelRequested: the job is running; its worker will wrap up and settle it.
	CancelRequested = "requested"
	// CancelNoop: there was nothing open to stop (already finished, or gone).
	CancelNoop = "noop"
)

// cancelledBeforeStartNote is the result recorded for a job stopped before any
// work began — an honest record rather than an error, since nothing went wrong.
const cancelledBeforeStartNote = "Stopped before it started."

// RequestAgentTaskCancel records a request to stop an open job, settling it
// immediately when nothing is in flight.
//
// Idempotent: an existing request's timestamp and requester are preserved
// (COALESCE), so a second click can't rewrite who stopped it. The state CASEs
// read the row's PRE-update state, so a 'running' job keeps running (its worker
// finalizes it) while a queued/parked job becomes terminal here.
//
// Returns one of CancelStopped / CancelRequested / CancelNoop.
func RequestAgentTaskCancel(ctx context.Context, id, by uuid.UUID) (string, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_agent_tasks
		SET cancel_requested_at = COALESCE(cancel_requested_at, now()),
		    cancel_requested_by = COALESCE(cancel_requested_by, $2),
		    state = CASE WHEN state IN ('queued','awaiting_input') THEN 'cancelled' ELSE state END,
		    result = CASE WHEN state IN ('queued','awaiting_input') THEN COALESCE(result, $3) ELSE result END,
		    ended_at = CASE WHEN state IN ('queued','awaiting_input') THEN now() ELSE ended_at END,
		    lease_token = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_token END,
		    lease_expires_at = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_expires_at END,
		    updated_at = now()
		WHERE id=$1 AND state IN ('queued','running','awaiting_input')
		RETURNING state`
	var newState string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, by, cancelledBeforeStartNote).Scan(&newState)
	if errors.Is(err, sql.ErrNoRows) {
		return CancelNoop, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RequestAgentTaskCancel err: %+v", err)
		return "", err
	}
	if newState == TaskCancelled {
		return CancelStopped, nil
	}
	return CancelRequested, nil
}

// RequestAgentTasksCancel is RequestAgentTaskCancel for every open job of an
// agent, as pausing or deleting it means. It answers the jobs it touched.
func RequestAgentTasksCancel(ctx context.Context, agentID, by uuid.UUID) ([]uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_agent_tasks
		SET cancel_requested_at = COALESCE(cancel_requested_at, now()),
		    cancel_requested_by = COALESCE(cancel_requested_by, $2),
		    state = CASE WHEN state IN ('queued','awaiting_input') THEN 'cancelled' ELSE state END,
		    result = CASE WHEN state IN ('queued','awaiting_input') THEN COALESCE(result, $3) ELSE result END,
		    ended_at = CASE WHEN state IN ('queued','awaiting_input') THEN now() ELSE ended_at END,
		    lease_token = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_token END,
		    lease_expires_at = CASE WHEN state IN ('queued','awaiting_input') THEN NULL ELSE lease_expires_at END,
		    updated_at = now()
		WHERE agent_id=$1 AND state IN ('queued','running','awaiting_input')
		RETURNING id`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, agentID, by, cancelledBeforeStartNote)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RequestAgentTasksCancel err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AgentTaskCancelRequested reports whether a stop has been requested for a job.
// Called by the worker's lease heartbeat (which is already polling), so it is a
// single indexed existence check and never mutates state. An error is treated as
// "no request" by the caller: a transient DB fault must not silently kill a
// healthy run.
func AgentTaskCancelRequested(ctx context.Context, id uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT EXISTS(SELECT 1 FROM ai_agent_tasks WHERE id=$1 AND cancel_requested_at IS NOT NULL)`
	var requested bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id).Scan(&requested); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AgentTaskCancelRequested err: %+v", err)
		return false, err
	}
	return requested, nil
}

// CancelLeasedAgentTask writes the terminal 'cancelled' row for a job THIS
// worker still leases, recording what the agent had managed to do before it
// stopped. Lease-guarded like every other terminal transition, so a worker whose
// lease was reclaimed can't clobber the new owner.
//
// note is the human-facing record (e.g. "Stopped by Akash — no changes were
// left half-applied."); partial is whatever the run produced before unwinding,
// which is worth keeping: a stopped run that already opened a PR or posted a
// comment must not look like it did nothing.
func CancelLeasedAgentTask(ctx context.Context, id, leaseToken uuid.UUID, note, partial string, runID *uuid.UUID) error {
	result := strings.TrimSpace(partial)
	note = strings.TrimSpace(note)
	switch {
	case result == "":
		result = note
	case note != "":
		result = note + "\n\n" + result
	}
	return FinishAgentTask(ctx, id, leaseToken, TaskCancelled, result, "", runID)
}

// GetAgentTaskByID reads one durable job. Used by the cancel path, which has to
// authorize the caller against the job's own people (who asked for it, who it
// runs as) before it may stop it — an id alone must never be enough.
func GetAgentTaskByID(ctx context.Context, id uuid.UUID) (*AgentTask, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `SELECT ` + agentTaskColumns + ` FROM ai_agent_tasks WHERE id=$1`
	t, err := scanAgentTask(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetAgentTaskByID err: %+v", err)
		return nil, err
	}
	return t, nil
}
