package Calendar

// Task blocks (calendar B1, gcal-new-task-blocks): time on the grid for a
// task. A block is an event of kind task_block that names its task
// (486_calendar_task_blocks): made through the one write path (Create), busy
// like any opaque event, and read with its task as the reader may read it:
// the task's name, where it stands and its project, from the task read that
// checks access (business/Task ReadableTasks), once for every block in a read.
// A block given no title keeps none (audit C17): wherever it is read, it is
// called by its task's name for a reader who may read the task, and "Task
// block" for anyone else (titleOf), so a restricted task's name never reaches
// a calendar's other readers and a renamed task's blocks follow it.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// readableTasks is the task read that checks access, and uidOf a reader's
// uid in the work graph, by which it decides; seams for tests.
var (
	readableTasks = taskBusiness.ReadableTasks
	uidOf         = accessBusiness.UIDOf
)

// taskBlockTitle is what a block given no title is called for a reader who
// may not read its task.
const taskBlockTitle = "Task block"

// titleOf is an event's title as a reader reads it, given its task as they
// may read it (tasksOf): the title it was given; else, for a task block, its
// task's name, or "Task block" when they may not read the task. Pure.
func titleOf(e *models.Event, task *dgraphStruct.EventTask) string {
	if e.Title != "" || e.TaskUUID == nil {
		return e.Title
	}
	if task != nil && task.Name != "" {
		return task.Name
	}
	return taskBlockTitle
}

// titler is titleOf for reader over events, whose blocks' tasks are read
// once, and only when a block among them is called by its task.
func (s *service) titler(ctx context.Context, reader accessBusiness.Subject, events []*models.Event) func(*models.Event) string {
	var named []*models.Event
	for _, e := range events {
		if e.TaskUUID != nil && e.Title == "" {
			named = append(named, e)
		}
	}
	tasks := map[uuid.UUID]*dgraphStruct.EventTask{}
	if len(named) > 0 {
		tasks = s.tasksOf(ctx, reader, named)
	}
	return func(e *models.Event) string {
		if e.TaskUUID == nil {
			return e.Title
		}
		return titleOf(e, tasks[*e.TaskUUID])
	}
}

// keptTitle is the title an update keeps for a task block called by its
// task: none while the title sent is the one it is read with (a form or a
// move sends back what it showed), so no update stores a copy of the task's
// name. A title of the writer's own is kept.
func (s *service) keptTitle(ctx context.Context, who *model.UserInfo, before, next *models.Event) string {
	if next.TaskUUID == nil || before.Title != "" || next.Title == "" {
		return next.Title
	}
	if next.Title == s.titler(ctx, subjectOf(who), []*models.Event{before})(before) {
		return ""
	}
	return next.Title
}

// blockedTask is the task a new block is time for, a task the writer may
// read now, and its name.
func (s *service) blockedTask(ctx context.Context, who *model.UserInfo, id string) (uuid.UUID, string, error) {
	task, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || !helpers.IsCanonicalUUID(strings.TrimSpace(id)) {
		return uuid.Nil, "", inputError("A task block needs its task.")
	}
	uid, err := uidOf(ctx, subjectOf(who))
	if err != nil {
		return uuid.Nil, "", err
	}
	found, err := readableTasks(ctx, []string{task.String()}, uid)
	if err != nil {
		return uuid.Nil, "", err
	}
	t, ok := found[task.String()]
	if !ok {
		return uuid.Nil, "", inputError("That task isn't one you can see.")
	}
	return task, t.Name, nil
}

// tasksOf is each block's task among events, by task uuid, as the reader may
// read it: one read for them all. A task the reader may no longer read is
// named by its uuid alone, so its block still says it is a task block. A read
// that fails leaves the blocks with their uuids: a task's name never fails
// the calendar's read.
func (s *service) tasksOf(ctx context.Context, reader accessBusiness.Subject, events []*models.Event) map[uuid.UUID]*dgraphStruct.EventTask {
	out := map[uuid.UUID]*dgraphStruct.EventTask{}
	ids := []string{}
	for _, e := range events {
		if e.TaskUUID == nil || out[*e.TaskUUID] != nil {
			continue
		}
		out[*e.TaskUUID] = &dgraphStruct.EventTask{UUID: e.TaskUUID.String()}
		ids = append(ids, e.TaskUUID.String())
	}
	if len(ids) == 0 {
		return out
	}
	uid, err := uidOf(ctx, reader)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar tasksOf: no graph uid: %v", err)
		return out
	}
	found, err := readableTasks(ctx, ids, uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar tasksOf: tasks not read: %v", err)
		return out
	}
	for id, t := range out {
		if r, ok := found[id.String()]; ok {
			t.Name, t.Status, t.ProjectUUID, t.ProjectName = r.Name, r.Status, r.ProjectUUID, r.ProjectName
		}
	}
	return out
}

// withTasks puts each block's task, and the title it is read with, on the
// answers made from rows (legacy.go).
func (s *service) withTasks(ctx context.Context, reader *model.UserInfo, rows []instance, out []*dgraphStruct.DgraphEvent) {
	events := make([]*models.Event, 0, len(rows))
	for _, inst := range rows {
		events = append(events, inst.Event)
	}
	tasks := s.tasksOf(ctx, subjectOf(reader), events)
	for i, inst := range rows {
		if inst.Event.TaskUUID != nil && i < len(out) {
			task := tasks[*inst.Event.TaskUUID]
			out[i].Task, out[i].Title = task, titleOf(inst.Event, task)
		}
	}
}
