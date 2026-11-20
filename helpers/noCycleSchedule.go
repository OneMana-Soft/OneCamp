package helpers

import (
	"context"

	"github.com/google/uuid"
)

// noCycleScheduleKey holds the project a context marks as having no cycle
// schedule.
type noCycleScheduleKey struct{}

// WithNoCycleSchedule marks ctx as having read, in this request's own write,
// that project runs its cycles by hand (business/Task setStatus, from the
// status write's own answer): what the change sets going through ctx (its
// task.status_changed listeners) needn't read the schedule again. Only a read
// made in this request marks it; a dispatcher without the mark reads as
// before. The mark names its project because ctx goes on to what the event
// sets going, which may be about another project's task: that one still reads.
func WithNoCycleSchedule(ctx context.Context, project uuid.UUID) context.Context {
	return context.WithValue(ctx, noCycleScheduleKey{}, project)
}

// NoCycleSchedule is whether ctx marks project as having no cycle schedule
// (WithNoCycleSchedule).
func NoCycleSchedule(ctx context.Context, project uuid.UUID) bool {
	p, ok := ctx.Value(noCycleScheduleKey{}).(uuid.UUID)
	return ok && project != uuid.Nil && p == project
}
