package business

// Started tasks join the running cycle (linear-12, as Linear's "auto-add
// started issues"): in a project whose cycles run themselves, with its
// schedule's add_started on, a task that starts (from Backlog or To do into In
// progress or In review) joins the cycle running then, unless it is in a cycle
// already, a later one included: planned work stays where it was planned.
//
// It listens for task.status_changed, so no task write path knows of cycles;
// the event is the server's own, naming the task and its project, and nothing
// from a client is read here. The change was already allowed by the task's own
// rule (Work Access); joining the cycle is the project's setting, which only
// those who manage its cycles set (SetCycleSchedule). A restricted task's
// events never reach this listener (business/Webhook), so a task few may see
// never shows in a cycle's counts by itself.

import (
	"context"
	"time"

	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	scheduleOfProject = cycleModel.ScheduleOf
	cyclesOfProject   = cycleModel.List
	cycleOfTask       = cycleModel.CycleOf
	putTaskInCycle    = cycleModel.SetTask
)

func init() {
	webhookBusiness.RegisterEventListener(startedJoinsCycle)
}

// starts says whether a move from old to next starts a task. Pure.
// It compares the built-in status ids, so for a project's own statuses it
// relies on the task.status_changed event naming each status's category
// (backlog, todo, in progress, in review) rather than the custom status id.
// If the event ever carries custom ids, read the status group here instead.
func starts(old, next string) bool {
	notYet := old == dgraphStruct.TASK_STATUS_BACKLOG || old == dgraphStruct.TASK_STATUS_TODO
	started := next == dgraphStruct.TASK_STATUS_INPROGRESS || next == dgraphStruct.TASK_STATUS_INREVIEW
	return notYet && started
}

// runningAt is the project's cycles running at now: two or more only
// where its cycles run side by side (parallel cycles). Pure.
func runningAt(cycles []cycleModel.Cycle, now time.Time) []cycleModel.Cycle {
	var out []cycleModel.Cycle
	for _, c := range cycles {
		if State(c, now) == "current" {
			out = append(out, c)
		}
	}
	return out
}

func startedJoinsCycle(ctx context.Context, eventType string, data map[string]interface{}) {
	if eventType != "task.status_changed" {
		return
	}
	str := func(k string) string { s, _ := data[k].(string); return s }
	if !starts(str("old_status"), str("new_status")) {
		return
	}
	project, err := uuid.Parse(str("project_id"))
	if err != nil {
		return
	}
	// The status change's own write found the project has no schedule
	// (business/Task setStatus): nothing to join, and nothing to read again.
	if helpers.NoCycleSchedule(ctx, project) {
		return
	}
	task, err := uuid.Parse(str("task_id"))
	if err != nil {
		return
	}
	if err := joinRunningCycle(ctx, project, task, time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/startedJoinsCycle task %s project %s: %v", task, project, err)
	}
}

// joinRunningCycle puts a task that started into its project's running cycle,
// when the project's schedule says so, the task is in no cycle yet and one
// cycle runs (a scheduled project's cycles never run side by side, but a
// schedule set before is no promise of that, so this reads them all).
func joinRunningCycle(ctx context.Context, project, task uuid.UUID, now time.Time) error {
	s, err := scheduleOfProject(ctx, project)
	if err != nil || s == nil || !s.AddStarted {
		return err
	}
	cycles, err := cyclesOfProject(project)
	if err != nil {
		return err
	}
	// With two running side by side, which one is the person's call.
	running := runningAt(cycles, now)
	if len(running) != 1 {
		return nil
	}
	in, err := cycleOfTask(task)
	if err != nil || in != nil {
		return err
	}
	return putTaskInCycle(task, &running[0].Id)
}
