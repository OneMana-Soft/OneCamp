package Domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
)

// TaskDeleted is whether the task was deleted (false for one that isn't
// there at all).
func TaskDeleted(ctx context.Context, taskUUID uuid.UUID) (bool, error) {
	deleted, err := models.GetTaskDeleted(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TaskDeleted Failed err: %+v", err)
	}
	return deleted, err
}
