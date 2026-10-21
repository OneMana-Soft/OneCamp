package business

import (
	"context"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// settleKey numbers a task made in a project that had no key yet (number 0):
// the project gets its key and the task its number now, rather than when the
// projects.keys sweep reaches it. A failure is logged: the task is made, and
// the sweep numbers it.
func settleKey(ctx context.Context, projectID uuid.UUID, number int) {
	if number > 0 {
		return
	}
	if err := taskKeys.Settle(ctx, projectID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Task couldn't number a new task in project %s: %+v", projectID, err)
	}
}
