package business

import (
	"context"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/TaskKey"
)

// SweepEvery is how often projects.keys runs: a project made before keys
// existed has its key, and its tasks their numbers, within this of the new
// code starting.
const SweepEvery = 2 * time.Minute

// sweepBatch is how many projects one pass settles of each kind.
const sweepBatch = 200

// Seams for tests.
var (
	projectsWithoutKeys = model.ProjectsWithoutKeys
	projectsUnnumbered  = model.ProjectsWithUnnumberedTasks
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "projects.keys", Every: SweepEvery, Run: sweep})
}

// sweep gives every project without a key its key, then numbers the tasks
// that have none in projects that have one. One project that fails is logged
// and the rest go on; the next pass tries it again.
func sweep(ctx context.Context) error {
	unkeyed, err := projectsWithoutKeys(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, p := range unkeyed {
		if _, err := Ensure(ctx, p.ID, p.Name); err != nil {
			helpers.LogErrorWithContext(ctx, "business/TaskKeys sweep couldn't give project %s a key: %+v", p.ID, err)
		}
	}
	unnumbered, err := projectsUnnumbered(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, id := range unnumbered {
		if _, err := numberProject(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/TaskKeys sweep couldn't number project %s's tasks: %+v", id, err)
		}
	}
	return nil
}
