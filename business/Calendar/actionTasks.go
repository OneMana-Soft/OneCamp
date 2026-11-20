package Calendar

// "Make task" from an action item of an event's call's recap
// (gcal-new-event-notes). The calendar's route names the event, the call and
// the item; business/AI makes the task (OnMakeActionTask, at its init) under
// the event's change rule and puts it on the event through AddFile. Only an
// edition with AI keeps a call's action items: one without has no maker, and
// every item is not found, so the calendar never imports AI code.

import (
	"context"
	"errors"

	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// ActionTask is the task an action item of an event's call made.
type ActionTask struct {
	TaskUUID string `json:"task_uuid"`
	Title    string `json:"title"`
	// Made is false when the item had its task already: this is that one.
	Made bool `json:"made"`
	// Linked is whether the new task is on the event; false when the event
	// couldn't take it (it carries as many files as it may).
	Linked bool `json:"linked"`
}

var (
	// ErrNoActionItem is an action item the event's calls don't have: no
	// such call on the event, or no such item on it.
	ErrNoActionItem = errors.New("there's no such action item on this event")
	// ErrActionItemBusy is an item whose task someone is making now.
	ErrActionItemBusy = errors.New("that task is being made just now; look again in a moment")
	// ErrCantAddTasks is a project the person may not add tasks to; the
	// error that wraps it says which and why.
	ErrCantAddTasks = errors.New("you can't add tasks to that project")
)

// ActionTaskMaker makes the task of action item i of the call with this key
// on the event id names, in the project projectUUID names, as who.
type ActionTaskMaker func(ctx context.Context, who *model.UserInfo, id, key string, i int, projectUUID string) (*ActionTask, error)

// actionTaskMaker is business/AI's, nil in an edition without AI.
var actionTaskMaker ActionTaskMaker

// OnMakeActionTask is how business/AI says it makes action items' tasks.
func OnMakeActionTask(f ActionTaskMaker) { actionTaskMaker = f }

// MakeActionTask makes an action item's task through its maker: the
// calendar's refusals as they are, ErrCantAddTasks, ErrNoActionItem (always,
// in an edition with no maker) or ErrActionItemBusy.
func MakeActionTask(ctx context.Context, who *model.UserInfo, id, key string, i int, projectUUID string) (*ActionTask, error) {
	if actionTaskMaker == nil {
		return nil, ErrNoActionItem
	}
	return actionTaskMaker(ctx, who, id, key, i, projectUUID)
}
