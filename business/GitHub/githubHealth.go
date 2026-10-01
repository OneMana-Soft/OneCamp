package business

// Is the sync queue still doing work it cannot finish?
//
// THE DEFECT THIS WATCHES FOR. Every task edit anywhere used to queue a GitHub
// sync whether or not the task had anything to do with GitHub. Each one woke the
// worker, took a lookup to discover there was nothing to sync to, and burned its
// full retry budget with backoff before being marked failed. On this product's
// own instance that was 67 of 69 recorded failures, all reading "task has no
// linked GitHub issue or PR", while GitHub's secondary rate limiter began
// refusing the two writes that were real.
//
// The guard now lives in EnqueueGitHubSync. If it is ever removed or bypassed,
// these rows come back, and they are the cheapest possible thing to look for.
//
// WHAT IT DOES NOT PROVE. That syncs SUCCEED. A repository whose token has been
// revoked fails differently and shows up as ordinary failures, which this counts
// separately and reports without judgement, because a workspace can legitimately
// have a few.

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	syncQueueModel "github.com/akashc777/OneCamp/models/postgres/GitHubSyncQueue"
)

// doomedWindow is how far back to look. Long enough to catch a regression that
// shipped yesterday, short enough that history from before the fix stays out.
const doomedWindow = 24 * time.Hour

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "github-sync",
		Kind: helpers.CheckKindBehaviour,
		Describe: "No sync work is being queued for tasks that have no GitHub link. It does not prove syncs " +
			"succeed: a revoked token fails differently and is counted as an ordinary failure.",
		Probe: func(ctx context.Context) error {
			doomed, other, err := syncQueueModel.CountRecentFailures(ctx, time.Now().Add(-doomedWindow))
			if err != nil {
				return fmt.Errorf("could not read the sync queue: %w", err)
			}
			if doomed > 0 {
				return fmt.Errorf("%d sync attempts in the last day were queued for tasks with no GitHub link, "+
					"which means the link guard is no longer holding; each one costs a worker wake and its full "+
					"retry budget (%d other failures in the same window)", doomed, other)
			}
			return nil
		},
	})
}
