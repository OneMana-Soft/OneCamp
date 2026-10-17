package business

import (
	"context"
	"fmt"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// ErrTaskGone is a refresh of a task that was deleted. It is
// helpers.ErrNotFound, so a route answers it as a task that isn't there.
var ErrTaskGone = fmt.Errorf("this task was deleted: %w", helpers.ErrNotFound)

// RefreshableTask is nil when GitHub's copy may be applied to the task: a
// refresh writes the issue's title, state and comments onto it and tells its
// people of them, so a deleted task is refused as gone (ErrTaskGone), its
// sync state untouched. The sync that closes a deleted task's issue is the
// one thing that reads a deleted task's project (syncAllowed); the refresh
// went through the same guard, so it was let through too.
func RefreshableTask(ctx context.Context, taskUUID uuid.UUID) error {
	deleted, err := taskDeleted(ctx, taskUUID)
	if err != nil {
		return err
	}
	if deleted {
		return ErrTaskGone
	}
	return nil
}

// taskDeleted is a seam for tests.
var taskDeleted = taskDomain.TaskDeleted
