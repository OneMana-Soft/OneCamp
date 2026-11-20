package business

// Parallel cycles (jira-04 head C, migration 593): a project's admins may let
// its cycles share days, two teams' sprints side by side. Off is the default.
// A task stays in at most one cycle. The scheduler makes the next cycle only
// for a project whose cycles run one after another: a schedule and parallel
// cycles exclude each other, each refused while the other holds.

import (
	"context"
	"errors"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// Parallel is whether the project's cycles may share days: its people read it.
func Parallel(ctx context.Context, project uuid.UUID) (bool, error) {
	return cycleModel.Parallel(ctx, project)
}

// SetParallel turns the project's parallel cycles on or off, refused in
// words a project admin can act on: off while two open cycles share days
// (named), on while a schedule runs the cycles.
func SetParallel(ctx context.Context, project uuid.UUID, on bool, by uuid.UUID) error {
	err := cycleModel.SetParallel(ctx, project, on, by)
	var shared *cycleModel.SharedDaysError
	switch {
	case errors.As(err, &shared):
		return &CycleError{shared.A.Label() + " and " + shared.B.Label() + " share days. Complete or delete one of them, then turn parallel cycles off."}
	case errors.Is(err, cycleModel.ErrScheduled):
		return &CycleError{"This project's cycles run on a schedule, one after another. Stop the schedule, then turn parallel cycles on."}
	}
	return err
}

// refusedSchedule is setting a schedule while the project's cycles run side
// by side: the scheduler makes the next cycle only after the last one.
const refusedSchedule = "This project runs parallel cycles, and a schedule makes them one after another. Turn parallel cycles off to set one."
