package business

import (
	"context"
	"strings"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// Planning a cycle (linear-13): putting tasks in it, one or many at once, and
// the capacity a person sets for it.

// CapacityMost is the most capacity a cycle takes, in minutes: 100,000 hours
// (migration 926's CHECK); CapacityMostPoints in points (migration 933's).
const (
	CapacityMost       = 6_000_000
	CapacityMostPoints = 100_000
)

// Placing is a task to put in a cycle, and the project it is in ("" for none).
type Placing struct {
	Task    uuid.UUID
	Project string
}

// Put puts tasks in the cycle with id cycleID, or takes them out of their
// cycles (cycleID ""), in one write however many. A task in no project, or in
// another project than the cycle's, is refused in words (refused); a cycle that
// isn't there or is complete refuses them all (a CycleError). Moved are the ids
// it moved; the rest were there already.
func Put(ctx context.Context, tasks []Placing, cycleID string) (moved []string, refused map[uuid.UUID]error, err error) {
	var target *cycleModel.Cycle
	if cycleID != "" {
		if id, perr := uuid.Parse(cycleID); perr == nil {
			if target, err = cycleModel.Get(id); err != nil {
				return nil, nil, err
			}
		}
		if target == nil {
			return nil, nil, &CycleError{"That isn't one of this project's cycles."}
		}
		if target.CompletedAt != nil {
			return nil, nil, &CycleError{"That cycle is complete. Pick a current or upcoming one."}
		}
	}
	refused = map[uuid.UUID]error{}
	var ids []uuid.UUID
	for _, p := range tasks {
		switch {
		case p.Project == "":
			refused[p.Task] = &CycleError{"A task in no project has no cycles."}
		case target != nil && !strings.EqualFold(target.ProjectUUID.String(), p.Project):
			refused[p.Task] = &CycleError{"That isn't one of this project's cycles."}
		default:
			ids = append(ids, p.Task)
		}
	}
	var to *uuid.UUID
	if target != nil {
		to = &target.Id
	}
	moved, err = cycleModel.SetTasks(ctx, ids, to)
	return moved, refused, err
}

// SetCapacity sets cycle c's own capacity in minutes, or in points for a
// project that estimates in them, or clears it (nil) so the project's, what
// its recent cycles finished, applies again. A complete cycle keeps the
// capacity it ran with.
func SetCapacity(ctx context.Context, c *cycleModel.Cycle, amount *int, inPoints bool) error {
	most, between, set := CapacityMost, "1 minute and 100,000 hours", cycleModel.SetCapacity
	if inPoints {
		most, between, set = CapacityMostPoints, "1 and 100,000 points", cycleModel.SetCapacityPoints
	}
	if amount != nil && (*amount < 1 || *amount > most) {
		return &CycleError{"A cycle's capacity is between " + between + "."}
	}
	if c.CompletedAt != nil {
		return &CycleError{"That cycle is complete. Its capacity stays as it ran."}
	}
	return set(ctx, c.Id, amount)
}
