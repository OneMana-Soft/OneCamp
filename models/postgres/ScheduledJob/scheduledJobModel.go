// Package models (ScheduledJob) is the Postgres data-access layer for the
// durable scheduler that backs /remind, scheduled messages, and recurring
// digests. The queue is claim-based and safe across restarts / multiple
// replicas via SELECT ... FOR UPDATE SKIP LOCKED.
package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Job status constants.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Job type constants.
const (
	JobTypeReminder         = "reminder"
	JobTypeScheduledMessage = "scheduled_message"
	JobTypeRecurringDigest  = "recurring_digest"
	JobTypeSavedItem        = "saved_item"
)

// ScheduledJob mirrors a row of the scheduled_jobs table.
type ScheduledJob struct {
	Id          uuid.UUID  `json:"id"`
	JobType     string     `json:"job_type"`
	UserUuid    uuid.UUID  `json:"user_uuid"`
	Payload     string     `json:"payload"` // raw JSON
	RunAt       time.Time  `json:"run_at"`
	Status      string     `json:"status"`
	Recurrence  *string    `json:"recurrence,omitempty"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	LastError   *string    `json:"last_error,omitempty"`
	LockedAt    *time.Time `json:"locked_at,omitempty"`
	LockedBy    *string    `json:"locked_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanFullJob(s scanner) (*ScheduledJob, error) {
	var j ScheduledJob
	var recurrence, lastError, lockedBy sql.NullString
	var lockedAt, deletedAt sql.NullTime

	err := s.Scan(
		&j.Id,
		&j.JobType,
		&j.UserUuid,
		&j.Payload,
		&j.RunAt,
		&j.Status,
		&recurrence,
		&j.Attempts,
		&j.MaxAttempts,
		&lastError,
		&lockedAt,
		&lockedBy,
		&j.CreatedAt,
		&j.UpdatedAt,
		&deletedAt,
	)
	if err != nil {
		return nil, err
	}
	if recurrence.Valid {
		j.Recurrence = &recurrence.String
	}
	if lastError.Valid {
		j.LastError = &lastError.String
	}
	if lockedBy.Valid {
		j.LockedBy = &lockedBy.String
	}
	if lockedAt.Valid {
		j.LockedAt = &lockedAt.Time
	}
	if deletedAt.Valid {
		j.DeletedAt = &deletedAt.Time
	}
	return &j, nil
}

// SelectColumns is the scan-coupled projection for a full scheduled_jobs row.
// Exported so the domain layer can compose the same column order when building
// the (dynamic) user-list query, keeping a single source of truth.
const SelectColumns = `id, job_type, user_uuid, payload, run_at, status, recurrence,
	attempts, max_attempts, last_error, locked_at, locked_by, created_at, updated_at, deleted_at`

// CreateJob inserts a new scheduled job and returns the generated id.
func CreateJob(ctx context.Context, jobType string, userUUID uuid.UUID, payloadJSON string, runAt time.Time, recurrence *string, maxAttempts int) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	id := uuid.New()
	const q = `INSERT INTO scheduled_jobs (id, job_type, user_uuid, payload, run_at, recurrence, max_attempts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, jobType, userUUID, payloadJSON, runAt, recurrence, maxAttempts)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateJob Failed to insert scheduled job err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// ClaimDueJobs atomically claims up to `limit` pending jobs whose run_at has
// passed (or whose lock is stale). FOR UPDATE SKIP LOCKED makes this safe to
// run from multiple workers/replicas concurrently — each row goes to exactly
// one claimer. The claim flips status→running and stamps locked_at/locked_by
// inside the same transaction.
func ClaimDueJobs(ctx context.Context, workerID string, now time.Time, staleBefore time.Time, limit int) ([]*ScheduledJob, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueJobs begin tx err: %+v", err)
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Select claimable rows: pending and due, OR running but with a stale lock
	// (a crashed worker). SKIP LOCKED ensures no two workers grab the same row.
	const selectQ = `
		SELECT ` + SelectColumns + `
		FROM scheduled_jobs
		WHERE deleted_at IS NULL
		  AND run_at <= $1
		  AND (
		        status = 'pending'
		     OR (status = 'running' AND (locked_at IS NULL OR locked_at < $2))
		  )
		ORDER BY run_at ASC
		LIMIT $3
		FOR UPDATE SKIP LOCKED`

	rows, err := tx.QueryContext(dbctx, selectQ, now, staleBefore, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueJobs select err: %+v", err)
		return nil, err
	}

	var jobs []*ScheduledJob
	var ids []uuid.UUID
	for rows.Next() {
		j, scanErr := scanFullJob(rows)
		if scanErr != nil {
			rows.Close()
			helpers.LogErrorWithContext(ctx, "models/ClaimDueJobs scan err: %+v", scanErr)
			return nil, scanErr
		}
		jobs = append(jobs, j)
		ids = append(ids, j.Id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		_ = tx.Commit()
		return nil, nil
	}

	const claimQ = `
		UPDATE scheduled_jobs
		SET status = 'running', locked_at = $1, locked_by = $2,
		    attempts = attempts + 1, updated_at = NOW()
		WHERE id = ANY($3)`
	if _, err := tx.ExecContext(dbctx, claimQ, now, workerID, pq.Array(ids)); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueJobs claim update err: %+v", err)
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueJobs commit err: %+v", err)
		return nil, err
	}

	// Reflect the claim in the returned structs.
	for _, j := range jobs {
		j.Status = StatusRunning
		j.Attempts++
	}
	return jobs, nil
}

// MarkDone flips a one-shot job to done.
func MarkDone(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE scheduled_jobs SET status = 'done', locked_at = NULL, locked_by = NULL, updated_at = NOW() WHERE id = $1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkDone err: %+v", err)
	}
	return err
}

// Reschedule moves a recurring job to its next run time and returns it to
// pending. attempts is reset to 0 because a successful firing starts a fresh
// retry budget for the next occurrence — otherwise a long-lived daily/weekly
// reminder would exhaust max_attempts after a handful of successful runs and
// then permanently fail on the first transient error.
func Reschedule(ctx context.Context, id uuid.UUID, nextRunAt time.Time) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE scheduled_jobs SET status = 'pending', run_at = $2, attempts = 0, locked_at = NULL, locked_by = NULL, last_error = NULL, updated_at = NOW() WHERE id = $1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, nextRunAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Reschedule err: %+v", err)
	}
	return err
}

// MarkFailedOrRetry sets last_error and either returns the job to pending (for
// a future retry with backoff) when attempts < max_attempts, or marks it failed.
func MarkFailedOrRetry(ctx context.Context, id uuid.UUID, errMsg string, retryAt time.Time) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `
		UPDATE scheduled_jobs
		SET last_error = $2,
		    status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		    run_at = CASE WHEN attempts >= max_attempts THEN run_at ELSE $3 END,
		    locked_at = NULL, locked_by = NULL, updated_at = NOW()
		WHERE id = $1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, errMsg, retryAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkFailedOrRetry err: %+v", err)
	}
	return err
}

// ExecJobs runs a pre-built scheduled-job list query (see domain/ScheduledJob,
// which owns the optional status filter + ordering/limit) and scans the rows.
func ExecJobs(ctx context.Context, query string, args []any) ([]*ScheduledJob, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecJobs err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var jobs []*ScheduledJob
	for rows.Next() {
		j, scanErr := scanFullJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
