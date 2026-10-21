package models

import (
	"context"

	"github.com/google/uuid"
)

// GetTaskDeleted is whether the task was deleted; false for a task that
// isn't there at all.
func GetTaskDeleted(ctx context.Context, taskUUID uuid.UUID) (bool, error) {
	return scalarOrZero[bool](ctx, `SELECT deleted_at IS NOT NULL FROM tasks WHERE id = $1`, taskUUID)
}
