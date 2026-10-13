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
	"errors"
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
	// drainMax bounds how many jobs one tick runs, so a backlog can't keep
	// the loop from its next tick (or from shutting down) for ever.
	drainMax = 500
	// lockTimeout is how long a claimed (running) job may stay locked before
	// another worker may reclaim it — covers a crashed worker. Jobs are
	// claimed one at a time, each just before it runs (DrainDueJobs), so a
	// live worker's lock is never older than one job's run (jobTimeout).
	lockTimeout = 5 * time.Minute
	// jobTimeout bounds one job's run, well inside lockTimeout.
	jobTimeout = 60 * time.Second
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
		DrainDueJobs(ctx, workerID)

		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Scheduler worker shutting down")
				return
			case <-ticker.C:
				DrainDueJobs(ctx, workerID)
			}
		}
	}()
}

// DrainDueJobs runs every job that is due, as worker, claiming each one just
// before it runs, and says how many it ran.
//
// One at a time, not in batches. The worker used to claim fifty jobs at once
// and run them in a line, each for up to a minute, while a lock older than
// five minutes could be taken by another worker: the jobs at the end of a
// slow batch were run by both, so a scheduled message or a reminder went out
// twice (BE-BUG-15). Claimed one at a time, a job's lock is never older than
// its own run, and other workers take the next ones meanwhile (SKIP LOCKED).
func DrainDueJobs(ctx context.Context, worker string) (ran int) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("Scheduler DrainDueJobs panic recovered: %v", r)
		}
	}()

	for ran < drainMax && ctx.Err() == nil {
		now := time.Now()
		jobs, err := jobModel.ClaimDueJobs(ctx, worker, now, now.Add(-lockTimeout), 1)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "Scheduler/ClaimDueJobs err: %+v", err)
			return ran
		}
		if len(jobs) == 0 {
			return ran
		}
		runJob(ctx, jobs[0])
		ran++
	}
	return ran
}

// runJob executes a single claimed job and records its outcome.
// Terminal is what a handler returns for a failure retrying cannot fix (a
// rule refused it, and would refuse again): the job is marked failed with the
// reason at once instead of being retried and then marked done.
type Terminal struct{ Reason string }

func (t *Terminal) Error() string { return t.Reason }

// RunClaimed runs a job the caller has already claimed (for example "send
// now"), with the same handler dispatch, timeout, retry and completion as the
// worker, so there is one way a job runs.
func RunClaimed(ctx context.Context, job *jobModel.ScheduledJob) { runJob(ctx, job) }

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

	// This run of the job already did what it does, and only marking it done
	// failed after (or its worker died there): do the marking, not the job.
	if alreadyRan(ctx, job) {
		helpers.LogInfoWithContext(ctx, "Scheduler/runJob job %s already ran at %s; finishing it without running it again", job.Id, job.RunAt)
		finishJob(ctx, job)
		return
	}

	// Bound each job's execution so a hung handler can't hold the claim.
	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	if err := handler(jobCtx, job); err != nil {
		var term *Terminal
		if errors.As(err, &term) {
			_ = jobModel.MarkFailed(ctx, job.Id, term.Reason)
			return
		}
		helpers.LogErrorWithContext(ctx, "Scheduler/runJob handler err (job %s): %+v", job.Id, err)
		_ = jobModel.MarkFailedOrRetry(ctx, job.Id, err.Error(), time.Now().Add(backoff(job.Attempts)))
		return
	}
	markRan(ctx, job)
	finishJob(ctx, job)
}

// finishJob records a run that did its work: a repeating job moves to its
// next time, any other is done.
func finishJob(ctx context.Context, job *jobModel.ScheduledJob) {
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
