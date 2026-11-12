package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// StatusChange is one status change in a task's history: by (a graph uid)
// moved it at at from prev to next, each named as the activity names it (a
// built-in status's key, or a project's own status's name as it was then).
// A move as it happens (UpdateTaskStatusByTaskUUID) and a data move that
// knows its history (the demo's finished work) write the same record. Pure.
func StatusChange(by string, at time.Time, prev, next string) *dgraphStruct.DgraphTaskActivity {
	return &dgraphStruct.DgraphTaskActivity{
		Uuid:      uuid.NewString(),
		Type:      dgraphStruct.ACTIVITY_TYPE_STATUS,
		CreatedBy: &dgraphStruct.DgraphUser{Uid: by},
		LogTime:   &at,
		PrevState: prev,
		NextState: next,
	}
}

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
	helpers.GoSafeLoop("business/Task/statusSince:StartStatusSinceBackfill", func() {
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
	})
}
