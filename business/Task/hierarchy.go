package business

// The work hierarchy's checks where tasks are made and changed (jira-01 J-H;
// the rule is domain/Task hierarchy.go): epic -> task -> sub-task, in one
// project. A sub-task is a task under a task that isn't an epic.

import (
	"context"
	"errors"

	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ErrHierarchy is a place in the hierarchy a task can't take, in words.
var ErrHierarchy = domain.ErrHierarchy

// Seams for tests: the hierarchy's read of tasks, and of a project's epics.
var (
	readPlaces = domain.ReadPlaces
	epicsIn    = domain.EpicsIn
)

// mayHoldNew is nil when a new task may be made under parent, of project:
// parent is an epic, or a task at the top or in an epic. A parent that
// isn't there is ErrTaskNotVisible.
func mayHoldNew(ctx context.Context, project, parent string) error {
	places, err := readPlaces(ctx, parent)
	if err != nil {
		return err
	}
	p, ok := places[parent]
	if !ok {
		return ErrTaskNotVisible
	}
	return domain.MayParent(domain.Place{Project: project}, &p)
}

// subTaskOf is whether the task named is a sub-task (under a task that isn't
// an epic). A task that isn't there is none.
func subTaskOf(places map[string]domain.Place, uuid string) bool {
	p, ok := places[uuid]
	return ok && p.SubTask()
}

// anySubTask is whether any of the tasks named, each with a parent, is a
// sub-task; a task in an epic is not one.
func anySubTask(ctx context.Context, uuids ...string) (bool, error) {
	places, err := readPlaces(ctx, uuids...)
	if err != nil {
		return false, err
	}
	for _, u := range uuids {
		if subTaskOf(places, u) {
			return true, nil
		}
	}
	return false, nil
}

// Reparent puts the task under parent (a task's uuid), or with parent "" out
// to the top, as user, on the one relation (domain ChangeParent). Both ids
// are read in the change's own transaction as live tasks: the person must be
// able to change the task and see the parent, by Work Access's link rule (as
// a dependency's: a parent they can't see is not found, nothing says it
// exists); then the hierarchy must take it there (domain MayParent: its own
// project, epic -> task -> sub-task).
func Reparent(ctx context.Context, taskID uuid.UUID, parent string, user *dgraphStruct.DgraphUser) error {
	_, err := domain.ChangeParent(ctx, taskID.String(), parent, user.Uid, func(c *domain.ParentChange) error {
		if c.Child == nil || c.ChildPlace == nil || c.Child.Project == nil {
			return ErrTaskNotVisible
		}
		if parent == "" {
			return mayChangeInTxn(ctx, c.Child, user)
		}
		if c.Parent == nil || c.ParentPlace == nil || c.Parent.Project == nil {
			return ErrTaskNotVisible
		}
		if err := mayLinkInTxn(ctx, c.Child, c.Parent, user); err != nil {
			if errors.Is(err, ErrDependencyMissing) {
				return ErrTaskNotVisible
			}
			return err
		}
		return domain.MayParent(*c.ChildPlace, c.ParentPlace)
	})
	return err
}
