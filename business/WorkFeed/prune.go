package business

// The Work Change Log keeps 30 days (WCL-1): the sweep projects.workchanges.prune
// (business/WorkSweep, hourly, one instance a pass) drops older changes in
// batches, oldest first. A reader whose place is older reads its lists again
// (ErrCursorExpired). PG12 never vacuums an insert-only table: these deletes
// are what gives autovacuum its work.

import (
	"context"
	"time"

	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
)

// keptFor is how long the log keeps a change.
const keptFor = 30 * 24 * time.Hour

// A pass deletes pruneBatch changes a statement, so no statement holds its
// locks long, and at most pruneMaxBatches of them; the next pass goes on.
const (
	pruneBatch      = 5000
	pruneMaxBatches = 200
)

// Seams: the oldest change to keep, and one batch's delete.
var (
	firstToKeep  = taskChangeModels.FirstToKeep
	deleteBefore = taskChangeModels.DeleteBefore
)

func init() {
	workSweep.WorkChangesPrune = func(ctx context.Context) error {
		_, err := prune(ctx, time.Now().Add(-keptFor))
		return err
	}
}

// prune deletes the changes made before cutoff, oldest first, pruneBatch a
// statement and at most pruneMaxBatches a pass, and answers how many went.
// It takes only those numbered before the first it keeps (FirstToKeep), so
// it never leaves a hole a reader could step over, and it always leaves the
// newest.
func prune(ctx context.Context, cutoff time.Time) (int64, error) {
	keepFrom, err := firstToKeep(ctx, cutoff)
	if err != nil || keepFrom <= 1 {
		return 0, err
	}
	var gone int64
	for i := 0; i < pruneMaxBatches; i++ {
		n, err := deleteBefore(ctx, keepFrom, pruneBatch)
		gone += n
		if err != nil || n < pruneBatch {
			return gone, err
		}
	}
	return gone, nil
}
