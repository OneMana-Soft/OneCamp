package business

import (
	"context"
	"sort"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	statusModel "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	"github.com/google/uuid"
)

// What a template keeps of a project, and what it leaves:
//
//   - its own statuses, all of them, so the board has the same columns;
//   - its tasks and their subtasks, except cancelled ones, with their names,
//     descriptions, priorities and tags;
//   - their dates, as days from the earliest date in the project, so the
//     spacing between them carries over to whenever the next project starts;
//   - not who they're assigned to, their comments, files or time: a template
//     is the plan, not the history.
//
// Every task starts again: one not yet started keeps its status (Backlog, To
// do, or a status of the project's own that counts as one of them), and the
// rest go back to To do.

// FromProject is a project's tasks and statuses as a template. Dates are read
// as days in loc, the zone of the person saving it. Pure.
func FromProject(name, description string, statuses []*statusModel.TaskStatus, tasks []*dgraphStruct.DgraphTask, loc *time.Location) Template {
	t := Template{Name: name, Description: description, Tasks: []Task{}}
	for _, s := range statuses {
		if s != nil {
			t.Statuses = append(t.Statuses, Status{Name: s.Name, Category: s.Category, Color: s.Color})
		}
	}
	keep := make([]*dgraphStruct.DgraphTask, 0, len(tasks))
	for _, task := range tasks {
		if kept(task) {
			keep = append(keep, task)
		}
	}
	// Tasks with a due date first, soonest first; then the rest in the order
	// they were made. That's the order a plan is read in.
	sort.SliceStable(keep, func(i, j int) bool {
		a, b := dateOf(keep[i].DueDate), dateOf(keep[j].DueDate)
		switch {
		case a == nil || b == nil:
			return a != nil && b == nil
		default:
			return a.Before(*b)
		}
	})
	zero := earliest(keep)
	for _, task := range keep {
		out := Task{Name: taskName(task.Name), Status: restart(task)}
		if ValidPriority(task.Priority) {
			out.Priority = task.Priority
		}
		if task.Description != nil {
			out.Description = *task.Description
		}
		if task.Label != nil {
			out.Tags = *task.Label
		}
		out.StartDay, out.DueDay = daysFrom(zero, task.StartDate, loc), daysFrom(zero, task.DueDate, loc)
		if out.StartDay != nil && out.DueDay != nil && *out.StartDay > *out.DueDay {
			out.StartDay = nil
		}
		for _, sub := range task.SubTasks {
			if kept(sub) {
				out.Subtasks = append(out.Subtasks, Subtask{Name: taskName(sub.Name), DueDay: daysFrom(zero, sub.DueDate, loc)})
			}
		}
		t.Tasks = append(t.Tasks, out)
	}
	return t
}

// kept: a template keeps every task and subtask that wasn't cancelled.
func kept(t *dgraphStruct.DgraphTask) bool {
	return t != nil && t.Status != dgraphStruct.TASK_STATUS_CANCELED
}

// taskName is a task's name as a template keeps it: on one line, cut to
// MaxTaskName, and never empty.
func taskName(name string) string {
	if name = helpers.OneLine(name, MaxTaskName); name == "" {
		return "Untitled task"
	}
	return name
}

// restart is the status a task starts again in.
func restart(task *dgraphStruct.DgraphTask) string {
	notStarted := task.Status == dgraphStruct.TASK_STATUS_BACKLOG || task.Status == dgraphStruct.TASK_STATUS_TODO
	switch {
	case notStarted && task.CustomStatusName != nil && *task.CustomStatusName != "":
		return *task.CustomStatusName
	case task.Status == dgraphStruct.TASK_STATUS_BACKLOG:
		return dgraphStruct.TASK_STATUS_BACKLOG
	default:
		return dgraphStruct.TASK_STATUS_TODO
	}
}

// dateOf is a date that is set; an unset one is the zero time, or nil.
func dateOf(d *time.Time) *time.Time {
	if d == nil || d.Year() <= 1970 {
		return nil
	}
	return d
}

// earliest is the first date on any task or subtask the template keeps, or
// nil when none has one. A cancelled subtask's date would start the plan on a
// day nothing in it has.
func earliest(tasks []*dgraphStruct.DgraphTask) *time.Time {
	var first *time.Time
	see := func(d *time.Time) {
		if d = dateOf(d); d != nil && (first == nil || d.Before(*first)) {
			first = d
		}
	}
	for _, task := range tasks {
		see(task.StartDate)
		see(task.DueDate)
		for _, sub := range task.SubTasks {
			if kept(sub) {
				see(sub.DueDate)
			}
		}
	}
	return first
}

// daysFrom is how many calendar days in loc d falls after zero, at most
// MaxDay, or nil when d isn't set.
func daysFrom(zero, d *time.Time, loc *time.Location) *int {
	if d = dateOf(d); d == nil || zero == nil {
		return nil
	}
	n := min(int(civil(*d, loc).Sub(civil(*zero, loc)).Hours()/24), MaxDay)
	return &n
}

// civil is the calendar day t falls on in loc, as midnight UTC, so days
// between two of them are whole whatever the zone's daylight saving does.
func civil(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Snapshot is a project as a template, read as its admin sees it. loc is the
// saver's zone.
func Snapshot(ctx context.Context, projectID uuid.UUID, name, description string, loc *time.Location) (Template, error) {
	p, err := projectDomain.GetDgraphProjectTasksForTemplate(ctx, projectID.String(), MaxTasks+1)
	if err != nil {
		return Template{}, err
	}
	statuses, err := taskStatusBusiness.List(ctx, projectID)
	if err != nil {
		return Template{}, err
	}
	fields, err := taskFieldBusiness.List(ctx, projectID)
	if err != nil {
		return Template{}, err
	}
	t := FromProject(name, description, statuses.Custom, p.Tasks, loc)
	for _, f := range fields {
		t.Fields = append(t.Fields, taskFieldBusiness.InputOf(f))
	}
	n := t.Size()
	if len(p.Tasks) > MaxTasks {
		n = max(n, int(p.TaskCount)) // only the first ones were read
	}
	if n > MaxTasks {
		return Template{}, fix("This project has %d tasks and subtasks; a template holds up to %d. Remove some, or save a smaller project as the template.", n, MaxTasks)
	}
	return Check(t)
}
