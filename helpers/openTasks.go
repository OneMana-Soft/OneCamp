package helpers

import (
	"context"

	"github.com/google/uuid"
)

// openTasksKey holds the tasks a context marks as restricted to no one.
type openTasksKey struct{}

// WithOpenTasks marks ctx as carrying tasks known to be restricted to no one
// (WA2): ones just made outside any restricted task (business/Task
// CreateTasks). What is said of them through ctx needn't read their
// restriction again. The mark names its tasks because ctx goes on to what
// their events set going (a workflow's actions, say), which may name other
// tasks: theirs is still read.
func WithOpenTasks(ctx context.Context, tasks ...uuid.UUID) context.Context {
	open := make(map[uuid.UUID]bool, len(tasks))
	for _, t := range tasks {
		open[t] = true
	}
	return context.WithValue(ctx, openTasksKey{}, open)
}

// TasksOpen is whether ctx marks every one of the tasks open
// (WithOpenTasks); false when it names none.
func TasksOpen(ctx context.Context, tasks ...string) bool {
	open, _ := ctx.Value(openTasksKey{}).(map[uuid.UUID]bool)
	for _, t := range tasks {
		id, err := uuid.Parse(t)
		if err != nil || !open[id] {
			return false
		}
	}
	return len(tasks) > 0
}
