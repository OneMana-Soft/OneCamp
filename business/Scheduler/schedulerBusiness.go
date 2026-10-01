// Package business (Scheduler) is the durable, claim-based job runner that
// backs /remind, scheduled messages, and recurring digests.
//
// Why a new primitive
// -------------------
// Until now the only time-delayed work in OneCamp was the calendar event
// created by the AI `set_reminder` executor — which does NOT deliver a nudge.
// Slack's /remind actually posts a message at the due time. This package adds
// that: a Postgres-backed queue (scheduled_jobs) drained by a ticker loop using
// SELECT ... FOR UPDATE SKIP LOCKED, so reminders survive restarts and are safe
// across multiple replicas (each due row is claimed by exactly one worker).
//
// Handlers are registered by feature packages (e.g. business/Command) via
// RegisterJobHandler, so this package has no upward dependencies.
package business

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
)

const (
	// pollInterval is how often the worker scans for due jobs. 15s keeps
	// reminders punctual without hammering Postgres (the partial index on
	// due+pending makes each scan cheap).
	pollInterval = 15 * time.Second
	// claimBatch bounds how many jobs one tick claims, so a backlog can't
	// stall the loop or exhaust DB connections.
	claimBatch = 50
	// lockTimeout is how long a claimed (running) job may stay locked before
	// another worker may reclaim it — covers a crashed worker.
	lockTimeout = 5 * time.Minute
	// workerHostnameFallback identifies this worker in locked_by for ops.
	workerHostnameFallback = "scheduler"
)

// JobHandler executes a single due job. It receives the parsed job and returns
// an error only for transient failures that should be retried; permanent
// failures (bad payload) should be logged and return nil so the job is not
// retried forever. A handler for a recurring job does NOT reschedule — the
// worker handles recurrence centrally from the job's Recurrence rule.
type JobHandler func(ctx context.Context, job *jobModel.ScheduledJob) error

var (
	handlerMu  sync.RWMutex
	handlers   = map[string]JobHandler{}
	workerID   string
	workerOnce sync.Once
)

// RegisterJobHandler wires a handler for a job_type. Called from feature
// packages' init() (e.g. business/Command registers "reminder").
func RegisterJobHandler(jobType string, h JobHandler) {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	handlers[jobType] = h
}

func getHandler(jobType string) (JobHandler, bool) {
	handlerMu.RLock()
	defer handlerMu.RUnlock()
	h, ok := handlers[jobType]
	return h, ok
}

// EnqueueInput describes a job to schedule.
type EnqueueInput struct {
	JobType     string
	UserUUID    uuid.UUID
	PayloadJSON string
	RunAt       time.Time
	Recurrence  *string // RRULE-lite, e.g. "FREQ=WEEKLY;BYDAY=MO"
	MaxAttempts int
}

// Enqueue persists a new job. Returns the job id.
func Enqueue(ctx context.Context, in EnqueueInput) (uuid.UUID, error) {
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return jobModel.CreateJob(ctx, in.JobType, in.UserUUID, in.PayloadJSON, in.RunAt, in.Recurrence, maxAttempts)
}

// StartWorker launches the background scheduler loop. Safe to call once at
// startup; ctx drives graceful shutdown. The loop is inert (just polls an
// empty result set cheaply) when there are no due jobs.
func StartWorker(ctx context.Context) {
	workerOnce.Do(func() {
		workerID = resolveWorkerID()
	})

	go func() {
		// Small initial delay so the DB pool is warm and migrations have run.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		// Drain once immediately on startup so jobs that came due while the
		// process was down fire promptly.
		drainDueJobs(ctx)

		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Scheduler worker shutting down")
				return
			case <-ticker.C:
				drainDueJobs(ctx)
			}
		}
	}()
}

// drainDueJobs claims and runs all currently-due jobs in batches.
func drainDueJobs(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("Scheduler drainDueJobs panic recovered: %v", r)
		}
	}()

	for {
		now := time.Now()
		staleBefore := now.Add(-lockTimeout)

		jobs, err := jobModel.ClaimDueJobs(ctx, workerID, now, staleBefore, claimBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "Scheduler/ClaimDueJobs err: %+v", err)
			return
		}
		if len(jobs) == 0 {
			return
		}

		for _, job := range jobs {
			runJob(ctx, job)
		}

		// If we filled the batch there may be more due jobs; loop again.
		if len(jobs) < claimBatch {
			return
		}
	}
}

// runJob executes a single claimed job and records its outcome.
func runJob(ctx context.Context, job *jobModel.ScheduledJob) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("Scheduler runJob panic (job %s): %v", job.Id, r)
			_ = jobModel.MarkFailedOrRetry(ctx, job.Id, "panic during execution", time.Now().Add(backoff(job.Attempts)))
		}
	}()

	handler, ok := getHandler(job.JobType)
	if !ok {
		// No handler registered for this type — mark failed so we stop
		// reclaiming it. This is a wiring bug, surfaced in last_error.
		helpers.LogErrorWithContext(ctx, "Scheduler/runJob no handler for type %s (job %s)", job.JobType, job.Id)
		_ = jobModel.MarkFailedOrRetry(ctx, job.Id, "no handler registered for job_type="+job.JobType, time.Now().Add(time.Hour))
		return
	}

	// Bound each job's execution so a hung handler can't hold the claim.
	jobCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := handler(jobCtx, job); err != nil {
		helpers.LogErrorWithContext(ctx, "Scheduler/runJob handler err (job %s): %+v", job.Id, err)
		_ = jobModel.MarkFailedOrRetry(ctx, job.Id, err.Error(), time.Now().Add(backoff(job.Attempts)))
		return
	}

	// Success. Reschedule recurring jobs; mark one-shot jobs done.
	if job.Recurrence != nil && *job.Recurrence != "" {
		next, rerr := NextRun(*job.Recurrence, time.Now(), job.RunAt)
		if rerr != nil || next.IsZero() {
			// Recurrence exhausted or invalid — treat as done.
			_ = jobModel.MarkDone(ctx, job.Id)
			return
		}
		_ = jobModel.Reschedule(ctx, job.Id, next)
		return
	}
	_ = jobModel.MarkDone(ctx, job.Id)
}

// backoff returns an exponential-ish retry delay capped at 30 minutes.
func backoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 1 * time.Minute
	case attempts == 2:
		return 5 * time.Minute
	case attempts == 3:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// resolveWorkerID builds a stable-ish worker identity for locked_by. Uses the
// hostname so multiple replicas are distinguishable in ops queries.
func resolveWorkerID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return workerHostnameFallback + "@" + h
	}
	return workerHostnameFallback + "@" + uuid.NewString()[:8]
}
