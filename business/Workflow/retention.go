package business

import (
	"context"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// RunsKept is how long a rule's run history is kept.
const RunsKept = 90 * 24 * time.Hour

// purgeBatch bounds one pass's deletes, so a big backlog goes over a few
// passes and never holds a long lock.
const purgeBatch = 5000

var purgeRuns = workflowModel.PurgeRuns

// purgeOldRuns deletes runs older than RunsKept, a batch at a time.
func purgeOldRuns(ctx context.Context) error {
	cutoff := clock().Add(-RunsKept)
	for i := 0; i < 20; i++ {
		n, err := purgeRuns(ctx, cutoff, purgeBatch)
		if err != nil || n < purgeBatch {
			return err
		}
	}
	return nil
}

func init() {
	// Claimed, so one instance runs each pass. Never renamed.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "workflows.runs.purge", Every: 6 * time.Hour, Run: purgeOldRuns})
}
