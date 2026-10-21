package business

import (
	"context"
	"errors"
	"time"

	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// The Scheduler's store and its pass (schedule.go plans it).

func init() {
	workSweep.Cycles = func(ctx context.Context) error { return RunSchedules(ctx, time.Now()) }
}

func scheduleOf(m cycleModel.Schedule) Schedule {
	s := Schedule{ProjectUUID: m.ProjectUUID, Weeks: m.Weeks, CooldownWeeks: m.CooldownWeeks, Upcoming: m.Upcoming, RollOver: m.RollOver, Anchor: m.Anchor}
	if m.SetBy != nil {
		s.By = *m.SetBy
	}
	return s
}

// GetSchedule is a project's schedule, or nil when its cycles run by hand.
func GetSchedule(ctx context.Context, project uuid.UUID) (*cycleModel.Schedule, error) {
	return cycleModel.ScheduleOf(ctx, project)
}

// SetSchedule sets a project's schedule and runs it at once, so the cycles it
// keeps ready are there when the person looks.
func SetSchedule(ctx context.Context, project uuid.UUID, in ScheduleInput, by uuid.UUID, now time.Time) (*cycleModel.Schedule, error) {
	s, err := in.Schedule()
	if err != nil {
		return nil, err
	}
	m := cycleModel.Schedule{ProjectUUID: project, Weeks: s.Weeks, CooldownWeeks: s.CooldownWeeks, Upcoming: s.Upcoming, RollOver: s.RollOver, Anchor: s.Anchor}
	if by != uuid.Nil {
		m.SetBy = &by
	}
	saved, err := cycleModel.SetSchedule(ctx, m)
	if err != nil {
		return nil, err
	}
	cycles, err := cycleModel.ListMany(ctx, []uuid.UUID{project})
	if err != nil {
		return nil, err
	}
	if err := runSchedule(ctx, scheduleOf(*saved), cycles[project], now); err != nil {
		return nil, err
	}
	return saved, nil
}

// StopSchedule hands a project's cycles back to its people; its cycles stay.
func StopSchedule(ctx context.Context, project uuid.UUID) (bool, error) {
	return cycleModel.DropSchedule(ctx, project)
}

// RunSchedules is the Sweeper's pass (projects.cycles, every 15 minutes, one
// instance): every scheduled project that is still there gets the cycles its
// schedule keeps ready, and its ended cycles completed. Three reads whatever
// the number of projects (the schedules, their projects in the graph, their
// cycles), then a write only for a project with something to do.
func RunSchedules(ctx context.Context, now time.Time) error {
	all, err := cycleModel.Schedules(ctx)
	if err != nil || len(all) == 0 {
		return err
	}
	ids := make([]string, len(all))
	projects := make([]uuid.UUID, len(all))
	for i, s := range all {
		ids[i], projects[i] = s.ProjectUUID.String(), s.ProjectUUID
	}
	live, err := projectDomain.LiveProjects(ctx, ids)
	if err != nil {
		return err
	}
	cycles, err := cycleModel.ListMany(ctx, projects)
	if err != nil {
		return err
	}
	var failed error
	for _, s := range all {
		if !live[s.ProjectUUID.String()] {
			continue
		}
		// One project's trouble is said and the others still run.
		if err := runSchedule(ctx, scheduleOf(s), cycles[s.ProjectUUID], now); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Cycle: the schedule of project %s didn't run: %+v", s.ProjectUUID, err)
			failed = err
		}
	}
	return failed
}

// runSchedule does one project's plan: the cycles to make, then the ended
// ones to complete (into the cycle now made after them).
func runSchedule(ctx context.Context, s Schedule, cycles []cycleModel.Cycle, now time.Time) error {
	// No one reads what a pass reads: a cycle's restricted tasks roll over too.
	ctx = taskread.WithSystem(ctx)
	plan := PlanSchedule(s, cycles, now)
	if len(plan.Create) > 0 {
		windows := make([]cycleModel.Window, len(plan.Create))
		for i, sl := range plan.Create {
			windows[i] = cycleModel.Window{StartsAt: sl.StartsAt, EndsAt: sl.EndsAt}
		}
		var by *uuid.UUID
		if s.By != uuid.Nil {
			by = &s.By
		}
		made, err := cycleModel.CreateMany(ctx, s.ProjectUUID, windows, by)
		if errors.Is(err, cycleModel.ErrOverlap) {
			// Someone made a cycle by hand since the read; the next pass plans around it.
			return nil
		}
		if err != nil {
			return err
		}
		cycles = append(cycles, made...)
	}
	byID := make(map[uuid.UUID]*cycleModel.Cycle, len(cycles))
	for i := range cycles {
		byID[cycles[i].Id] = &cycles[i]
	}
	for _, id := range plan.Complete {
		c := byID[id]
		if c == nil {
			continue
		}
		if _, err := Complete(ctx, c, s.RollOver, s.By); err != nil {
			var ce *CycleError
			if errors.As(err, &ce) {
				// Completed by hand meanwhile: nothing to do.
				continue
			}
			return err
		}
	}
	return nil
}
