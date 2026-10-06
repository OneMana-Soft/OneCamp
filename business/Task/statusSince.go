package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// StatusSinceOf works out when a task entered its current status, for tasks
// from before task_status_since was kept: its latest status change, or its
// creation when it never changed, or now when neither is known.
func StatusSinceOf(t *dgraphStruct.DgraphTask, now time.Time) time.Time {
	var latest time.Time
	for _, a := range t.Activity {
		if a != nil && a.Type == dgraphStruct.ACTIVITY_TYPE_STATUS && a.LogTime != nil && a.LogTime.After(latest) {
			latest = *a.LogTime
		}
	}
	if !latest.IsZero() {
		return latest
	}
	if t.CreatedAt != nil && t.CreatedAt.Year() > 1970 {
		return *t.CreatedAt
	}
	return now
}

const statusSinceBatch = 200

// StartStatusSinceBackfill gives every task that lacks one its
// task_status_since, once, in the background. New tasks and every status
// change set it as they happen, so after the first start this finds nothing.
func StartStatusSinceBackfill(ctx context.Context) {
	go func() {
		done := 0
		// A ceiling, so a write that silently does not apply cannot loop forever.
		for round := 0; round < 10_000; round++ {
			if ctx.Err() != nil {
				return
			}
			tasks, err := domain.GetDgraphTasksWithoutStatusSince(ctx, statusSinceBatch)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/StartStatusSinceBackfill read err: %+v", err)
				return
			}
			if len(tasks) == 0 {
				break
			}
			now := time.Now()
			for _, t := range tasks {
				since := StatusSinceOf(t, now)
				t.StatusSince = &since
			}
			if err := domain.SetDgraphTaskStatusSince(ctx, tasks); err != nil {
				helpers.LogErrorWithContext(ctx, "business/StartStatusSinceBackfill write err: %+v", err)
				return
			}
			done += len(tasks)
		}
		if done > 0 {
			helpers.MessageLogs.InfoLog.Printf("time in status: backfilled %d tasks", done)
		}
	}()
}
