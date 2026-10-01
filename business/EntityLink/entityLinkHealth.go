package entityLinkBusiness

// Does the soft-delete filter still match anything?
//
// THE DEFECT THIS WATCHES FOR. Every filter in this package once tested
// soft-deletion by EXISTENCE, `not has(task_deleted_at)`. Live rows here carry
// the Go zero time rather than omitting the predicate, so has() is true for
// everything ever created and the filter matched NOTHING. The source lookup
// returned no rows, so every task and project answered "you are not a member"
// regardless of who asked, linked docs and boards were filtered out of their own
// results, and creating a link silently did nothing. Nothing errored. It behaved
// that way from the day the feature shipped until it was found by hand.
//
// The signature is simple and cheap to look for: a workspace that has tasks, for
// which the filter matches none, is a workspace whose filter is broken again.
//
// WHAT IT DOES NOT PROVE. That links resolve correctly, only that the predicate
// they all depend on still selects live rows. An empty workspace is reported
// healthy because there is nothing to be wrong about, not because it was checked.

import (
	"context"
	"fmt"

	domain "github.com/akashc777/OneCamp/domain/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "entity-links",
		Kind: helpers.CheckKindBehaviour,
		Describe: "The soft-delete filter that every link query depends on still selects live rows. It does not " +
			"prove links resolve correctly, and a workspace with no tasks is reported healthy because there is " +
			"nothing to be wrong about.",
		Probe: func(ctx context.Context) error {
			total, live, err := domain.CountTasksAndLiveTasks(ctx)
			if err != nil {
				return fmt.Errorf("could not query the task graph: %w", err)
			}
			if total == 0 {
				return nil
			}
			if live == 0 {
				return fmt.Errorf("this workspace has %d tasks and the soft-delete filter matches none of them, "+
					"so document and board links will silently return nothing", total)
			}
			return nil
		},
	})
}
