package business

// What someone may do, as the app shows it: project_access on every project
// read for them and task_access on a task, from the rule, so the web never
// guesses (lib/work/access.ts reads them).

import (
	"context"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Which tasks someone changes or deletes, as project_access says it.
const (
	TasksAll      = "all"
	TasksAssigned = "assigned"
	TasksOwn      = "own"
	TasksNone     = "none"
)

// Access is what standing s lets someone do in the project. Pure.
func (s Standing) Access() *dgraphStruct.ProjectAccess {
	edit := TasksNone
	if s.Can(NeedEditTask) {
		edit = TasksAll
		if s.Role != RoleAdmin && s.EditWho == EditAssigned {
			edit = TasksAssigned
		}
	}
	del := TasksNone
	switch {
	case s.Role == RoleAdmin:
		del = TasksAll
	case s.Can(NeedDeleteTask):
		del = TasksOwn
	}
	return &dgraphStruct.ProjectAccess{
		Role:         string(s.Role),
		View:         s.Can(NeedView),
		Comment:      s.Can(NeedComment),
		CreateTasks:  s.Can(NeedCreateTask),
		EditTasks:    edit,
		DeleteTasks:  del,
		LogTime:      s.Can(NeedLogTime),
		PostUpdates:  s.Can(NeedPostUpdate),
		AddFiles:     s.Can(NeedAddFiles),
		Manage:       s.Can(NeedManage),
		ManagePeople: s.Can(NeedManagePeople),
		Archive:      s.Can(NeedArchive),
	}
}

// TaskAccess is what standing s and facts f let someone do with the task.
// Pure.
func (s Standing) TaskAccess(f TaskFacts) *dgraphStruct.TaskAccess {
	return &dgraphStruct.TaskAccess{
		Edit:     s.CanOnTask(NeedEditTask, f),
		Delete:   s.CanOnTask(NeedDeleteTask, f),
		Comment:  s.CanOnTask(NeedComment, f),
		LogTime:  s.CanOnTask(NeedLogTime, f),
		Restrict: s.MayRestrict(f),
	}
}

// Decorate sets Access on each project read for the person, with one read of
// their roles.
func Decorate(ctx context.Context, userUUID string, ps ...*dgraphStruct.DgraphProject) error {
	standings, err := OfMany(ctx, userUUID, ps)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p != nil {
			p.Access = standings[strings.ToLower(p.Uuid)].Access()
		}
	}
	return nil
}

// DecorateTasks sets each task's own Access and its project's, for tasks read
// for the person (by uuid and graph uid), with one read of their roles
// however many projects the tasks are in.
func DecorateTasks(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) error {
	_, err := decorate(ctx, userUUID, userUID, ts...)
	return err
}

// DecorateTask is DecorateTasks for one task, also giving the reader's
// standing in its project (none for a task outside one), so a reader of the
// task and its project's settings needs no second read of their roles.
func DecorateTask(ctx context.Context, userUUID, userUID string, t *dgraphStruct.DgraphTask) (Standing, error) {
	standings, err := decorate(ctx, userUUID, userUID, t)
	if err != nil || t == nil || t.Project == nil {
		return Standing{}, err
	}
	return standings[strings.ToLower(t.Project.Uuid)], nil
}

func decorate(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) (map[string]Standing, error) {
	var projects []*dgraphStruct.DgraphProject
	for _, t := range ts {
		if t != nil && t.Project != nil {
			projects = append(projects, t.Project)
		}
	}
	standings, err := OfMany(ctx, userUUID, projects)
	if err != nil {
		return nil, err
	}
	facts, err := FactsForMany(ctx, userUUID, userUID, ts...)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		if t == nil {
			continue
		}
		var s Standing
		if t.Project != nil && (t.Project.Uuid != "" || t.Project.Uid != "") {
			s = standings[strings.ToLower(t.Project.Uuid)]
			t.Project.Access = s.Access()
		}
		t.Access = s.TaskAccess(facts[strings.ToLower(t.Uuid)])
	}
	return standings, nil
}
