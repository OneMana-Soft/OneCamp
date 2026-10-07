package business

// Dependencies: a task can wait on other tasks of its project (finish to
// start: it can start once they are done). The timeline draws them as arrows
// and, when a task moves later, moves the tasks waiting on it along
// (ShiftDependents); a board, a list and the timeline mark a task that is
// still waiting as blocked. Only a project's top-level tasks take part.

import (
	"context"
	"errors"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

var (
	ErrDependencySelf    = errors.New("a task can't wait on itself")
	ErrDependencyProject = errors.New("the tasks are in different projects")
	ErrDependencySubtask = errors.New("subtasks don't take dependencies")
	ErrDependencyLoop    = errors.New("the dependency would make a loop")
	ErrDependencyMissing = errors.New("task not found")
	ErrNotProjectAdmin   = errors.New("not one of the project's admins")
)

func live(t *dgraphStruct.DgraphTask) bool {
	return t != nil && t.Uuid != "" && (t.DeletedAt == nil || t.DeletedAt.Year() <= 1970)
}

// AddTaskDependency makes taskUUID wait on blockerUUID. Both must be live
// top-level tasks of one project, the person one of its admins, and the
// blocker must not already wait on the task, however indirectly. Adding a
// dependency that's already there changes nothing.
func AddTaskDependency(ctx context.Context, taskUUID, blockerUUID string, user *dgraphStruct.DgraphUser) error {
	if taskUUID == blockerUUID {
		return ErrDependencySelf
	}
	changed, err := domain.ChangeTaskDependency(ctx, taskUUID, blockerUUID, user.Uid, false, func(pair *domain.DependencyPair) error {
		if !live(pair.Task) || !live(pair.Blocker) {
			return ErrDependencyMissing
		}
		if pair.Task.ParentTask != nil || pair.Blocker.ParentTask != nil {
			return ErrDependencySubtask
		}
		if pair.Task.Project == nil || pair.Blocker.Project == nil || pair.Task.Project.Uuid != pair.Blocker.Project.Uuid {
			return ErrDependencyProject
		}
		if pair.Task.Project.IsProjectAdmin == 0 {
			return ErrNotProjectAdmin
		}
		if pair.Upstream[taskUUID] {
			return ErrDependencyLoop
		}
		return nil
	})
	if err != nil || !changed {
		return err
	}
	return recordDependency(ctx, taskUUID, user, dgraphStruct.ACTIVITY_TYPE_ADD_DEPENDENCY, "", blockerUUID)
}

// RemoveTaskDependency stops taskUUID waiting on blockerUUID. Either may
// since have been deleted; the person must be an admin of the task's project.
// Taking off a dependency that isn't there changes nothing.
func RemoveTaskDependency(ctx context.Context, taskUUID, blockerUUID string, user *dgraphStruct.DgraphUser) error {
	changed, err := domain.ChangeTaskDependency(ctx, taskUUID, blockerUUID, user.Uid, true, func(pair *domain.DependencyPair) error {
		if pair.Task == nil || pair.Task.Uuid == "" || pair.Task.Project == nil {
			return ErrDependencyMissing
		}
		if pair.Task.Project.IsProjectAdmin == 0 {
			return ErrNotProjectAdmin
		}
		return nil
	})
	if err != nil || !changed {
		return err
	}
	return recordDependency(ctx, taskUUID, user, dgraphStruct.ACTIVITY_TYPE_REMOVE_DEPENDENCY, blockerUUID, "")
}

// recordDependency writes the line in the waiting task's history.
func recordDependency(ctx context.Context, taskUUID string, user *dgraphStruct.DgraphUser, kind, before, after string) error {
	now := time.Now()
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID, UpdatedAt: &now, Activity: []*dgraphStruct.DgraphTaskActivity{{
		Uuid:      uuid.NewString(),
		Type:      kind,
		CreatedBy: &dgraphStruct.DgraphUser{Uid: user.Uid},
		LogTime:   &now,
		PrevState: before,
		NextState: after,
	}}}
	_, err := domain.CreateOrUpdateDgraphTask(ctx, write)
	return err
}

// ShiftedTask is a task that moved because a task it waits on did.
type ShiftedTask struct {
	UUID  string     `json:"task_uuid"`
	Start *time.Time `json:"task_start_date,omitempty"`
	Due   *time.Time `json:"task_due_date,omitempty"`

	before *dgraphStruct.DgraphTask
}

// maxShifted bounds how many tasks one move can push along.
const maxShifted = 500

// ShiftDependents moves the tasks waiting on movedUUID, and the tasks waiting
// on those, just far enough that each starts the day after the moved tasks it
// waits on are due (see PlanShifts). Only tasks of projectUUID move: it's the
// project the person was checked to run. Each move goes into its task's
// history. Days are in loc.
func ShiftDependents(ctx context.Context, projectUUID, movedUUID string, loc *time.Location, user *dgraphStruct.DgraphUser) ([]ShiftedTask, error) {
	all, err := domain.DownstreamSchedule(ctx, movedUUID)
	if err != nil {
		return nil, err
	}
	tasks := all[:0]
	for _, t := range all {
		if t.Project != nil && t.Project.Uuid == projectUUID {
			tasks = append(tasks, t)
		}
	}
	plan := PlanShifts(tasks, movedUUID, loc)
	done := make([]ShiftedTask, 0, len(plan))
	for _, s := range plan {
		id, err := uuid.Parse(s.UUID)
		if err != nil {
			continue
		}
		if err := UpdateTaskDates(ctx, id, TaskDates{Start: s.Start, Due: s.Due}, s.before, user); err != nil {
			return done, err
		}
		done = append(done, s)
	}
	return done, nil
}

// civil is t's calendar day in loc, as UTC midnight, so days subtract exactly.
func civil(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func dated(t *time.Time) bool { return t != nil && t.Year() > 1970 }

// lastDay is the day a task is done by: its due date, else its start.
func lastDay(t *dgraphStruct.DgraphTask, loc *time.Location) (time.Time, bool) {
	if dated(t.DueDate) {
		return civil(*t.DueDate, loc), true
	}
	if dated(t.StartDate) {
		return civil(*t.StartDate, loc), true
	}
	return time.Time{}, false
}

// firstDay is the day a task starts: its start date, else its due date.
func firstDay(t *dgraphStruct.DgraphTask, loc *time.Location) (time.Time, bool) {
	if dated(t.StartDate) {
		return civil(*t.StartDate, loc), true
	}
	if dated(t.DueDate) {
		return civil(*t.DueDate, loc), true
	}
	return time.Time{}, false
}

func closed(t *dgraphStruct.DgraphTask) bool {
	return t.Status == dgraphStruct.TASK_STATUS_DONE || t.Status == dgraphStruct.TASK_STATUS_CANCELED
}

// PlanShifts is ShiftDependents' arithmetic: which tasks move, and to what
// dates, given the tasks waiting on movedUUID however indirectly (and it,
// with its new dates). A task moves only when a task it waits on moved (the
// one moved, or one this plan moves) and is now due on or after the day it
// starts; it then starts the day after the last of those is due, keeping how
// long it runs and its times of day. Nothing moves earlier. A finished task
// neither moves nor holds up another, and a task without dates stays where
// it is (Asana's "keep the buffer", monday's "flexible"). A conflict the move
// didn't cause is left as it was.
//
// Tasks are planned in dependency order (Kahn's), each once, after every task
// it waits on in the chain: a task waiting on two moved tasks sees both at
// their new dates. A task caught in a loop (the server refuses one, but data
// written before that rule may hold one) never comes round, so it and the
// tasks after it stay where they are.
func PlanShifts(tasks []*dgraphStruct.DgraphTask, movedUUID string, loc *time.Location) []ShiftedTask {
	byID := make(map[string]*dgraphStruct.DgraphTask, len(tasks))
	for _, t := range tasks {
		byID[t.Uuid] = t
	}
	if byID[movedUUID] == nil {
		return nil
	}
	// Who waits on whom, among the tasks given.
	waiting := map[string][]string{}
	for _, t := range tasks {
		for _, b := range t.BlockedBy {
			if b != nil && b.Uuid != t.Uuid && byID[b.Uuid] != nil {
				waiting[b.Uuid] = append(waiting[b.Uuid], t.Uuid)
			}
		}
	}
	// The tasks downstream of the moved one, and how many of the tasks each
	// waits on are in the chain (the moved one, or downstream themselves).
	inChain := map[string]bool{movedUUID: true}
	queue := []string{movedUUID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, w := range waiting[id] {
			if !inChain[w] {
				inChain[w] = true
				queue = append(queue, w)
			}
		}
	}
	pending := map[string]int{}
	for id := range inChain {
		if id == movedUUID {
			continue
		}
		for _, b := range byID[id].BlockedBy {
			if b != nil && b.Uuid != id && inChain[b.Uuid] {
				pending[id]++
			}
		}
	}
	// What moved, at its new dates: the moved task, and each task planned to.
	moved := map[string]*dgraphStruct.DgraphTask{movedUUID: byID[movedUUID]}
	var order []string
	queue = []string{movedUUID}
	for len(queue) > 0 && len(order) < maxShifted {
		id := queue[0]
		queue = queue[1:]
		for _, w := range waiting[id] {
			if pending[w]--; pending[w] != 0 {
				continue
			}
			queue = append(queue, w)
			if next := shifted(byID[w], moved, loc); next != nil {
				moved[w] = next
				order = append(order, w)
			}
		}
	}
	out := make([]ShiftedTask, 0, len(order))
	for _, id := range order {
		m := moved[id]
		s := ShiftedTask{UUID: id, before: byID[id]}
		if dated(m.StartDate) {
			s.Start = m.StartDate
		}
		if dated(m.DueDate) {
			s.Due = m.DueDate
		}
		out = append(out, s)
	}
	return out
}

// shifted is t moved to start the day after the last of the moved, open
// tasks it waits on is due, or nil when it starts late enough already, is
// finished, or has no dates. It's a copy: t is left as it was.
func shifted(t *dgraphStruct.DgraphTask, moved map[string]*dgraphStruct.DgraphTask, loc *time.Location) *dgraphStruct.DgraphTask {
	if closed(t) {
		return nil
	}
	from, ok := firstDay(t, loc)
	if !ok {
		return nil
	}
	var need time.Time
	for _, b := range t.BlockedBy {
		if b == nil {
			continue
		}
		if bt := moved[b.Uuid]; bt != nil && !closed(bt) {
			if last, ok := lastDay(bt, loc); ok && !last.Before(need) {
				need = last.AddDate(0, 0, 1)
			}
		}
	}
	if !from.Before(need) {
		return nil
	}
	days := int(need.Sub(from).Hours() / 24)
	next := *t
	if dated(t.StartDate) {
		s := t.StartDate.In(loc).AddDate(0, 0, days)
		next.StartDate = &s
	}
	if dated(t.DueDate) {
		d := t.DueDate.In(loc).AddDate(0, 0, days)
		next.DueDate = &d
	}
	return &next
}
