package business

// Saying when a provider has paused an import, and when it carries on.
//
// monday.com's free and basic plans allow 1,000 API calls a day, and other
// providers have hourly quotas. An import that runs into one waits: each
// worker naps (two minutes at most, so its claim on a chunk isn't taken for a
// stuck one), asks again, and carries on by itself once the allowance is
// back. It used to wait in silence, a "running" import that didn't move for
// hours. A limit the provider says lasts longer than a nap is now written on
// the job (progress.paused_until and pause_reason) and the job's progress
// event goes out, so the import card says it is waiting and until when. The
// note is cleared as soon as a chunk gets done again.

import (
	"context"
	"errors"
	"sync"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// maxNap is the longest a worker sleeps on a limit before asking again.
const maxNap = 2 * time.Minute

// Seams, so the note is tested without a database or a broker.
var (
	saveProgress   = importModels.UpdateProgress
	announcePaused = func(ctx context.Context, job *importModels.Job) {
		publishProgress(ctx, job, importModels.StatusRunning, "waiting", "")
	}
)

// pausedJobs remembers, per job, that a pause was written down, so the note
// is cleared once, by the first chunk done after it.
var pausedJobs sync.Map // uuid.UUID → struct{}

// waitOutRateLimit notes a long limit on the job, then naps before the
// chunk is asked for again.
func waitOutRateLimit(ctx context.Context, job *importModels.Job, err error, d time.Duration) {
	notePause(ctx, job, err, d, time.Now())
	sleepUntilRetryAfter(ctx, d)
}

// notePause writes a limit that outlasts a nap on the job, with when the
// provider said it lifts, and tells the admin's screens.
func notePause(ctx context.Context, job *importModels.Job, err error, d time.Duration, now time.Time) {
	if job == nil || d <= maxNap {
		return
	}
	reason := "the provider is limiting requests"
	var rl *importProvider.ErrRateLimited
	if errors.As(err, &rl) && rl.Reason != "" {
		reason = rl.Reason
	}
	until := now.Add(d).UTC().Truncate(time.Second)
	if saveProgress(ctx, job.Id, mustMarshal(map[string]any{
		"paused_until": until.Format(time.RFC3339),
		"pause_reason": reason,
	})) != nil {
		return
	}
	if _, already := pausedJobs.LoadOrStore(job.Id, struct{}{}); !already {
		announcePaused(ctx, job)
	}
}

// clearPause removes the note once work goes on.
func clearPause(ctx context.Context, jobId uuid.UUID) {
	if _, was := pausedJobs.LoadAndDelete(jobId); !was {
		return
	}
	_ = saveProgress(ctx, jobId, mustMarshal(map[string]any{"paused_until": nil, "pause_reason": nil}))
}
