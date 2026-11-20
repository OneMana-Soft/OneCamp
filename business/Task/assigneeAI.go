package business

// The AI edition's part of a hand-over: the task's AI entry names its new
// assignee (ai.SetTaskAssignees). AI only: without-ai drops this file, and a
// hand-over then writes the task and its search entry alone.

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func init() {
	aiAssigneeWrite = func(ctx context.Context, to map[string]string) error {
		if !aiOn() {
			// AI off: nothing searches the AI index. An entry a hand-over
			// missed meanwhile is a miss when AI is on again, never a leak
			// (services/AI keepAssigned).
			return nil
		}
		_, err := setAIAssignees(ctx, to)
		if err != nil {
			retryAIAssignees(ctx, to)
		}
		return err
	}
}

// retryAIAssignees writes the tasks of a hand-over write that failed once
// more, in the background after aiAssigneeRetryWait, and only once: with
// their assignees as they are by then (one read), so a retry never writes
// back an assignee a later hand-over replaced. A retry that fails too is
// logged and left: the new assignee from outside the project misses the task
// in Ask AI, and no one else reads it (services/AI keepAssigned).
func retryAIAssignees(ctx context.Context, to map[string]string) {
	ids := make([]string, 0, len(to))
	for id := range to {
		ids = append(ids, id)
	}
	ctx = context.WithoutCancel(ctx)
	goRetry("task.ai-assignee-retry", func() {
		time.Sleep(aiAssigneeRetryWait)
		if !aiOn() {
			return
		}
		c, cancel := context.WithTimeout(ctx, aiAssigneeRetryDeadline)
		defer cancel()
		now, err := assigneesNow(c, ids)
		if err == nil {
			_, err = setAIAssignees(c, now)
		}
		if err != nil {
			helpers.LogErrorWithContext(c, "business/Task: %d tasks' assignees aren't on their AI entries after a retry: %v", len(ids), err)
		}
	})
}

// aiAssigneeRetryDeadline bounds a retry's read and write.
const aiAssigneeRetryDeadline = 30 * time.Second

// Seams for tests.
var (
	// aiOn is whether AI is on now, as business/Board/boardText.go asks it.
	aiOn = func() bool {
		s := ai.GetService()
		return s != nil && s.IsEnabled()
	}
	setAIAssignees      = ai.SetTaskAssignees
	assigneesNow        = domain.TaskAssignees
	goRetry             = helpers.GoSafeNamed
	aiAssigneeRetryWait = 10 * time.Second
)
