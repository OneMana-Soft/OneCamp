package business

// Undoing a run: each change it made goes back to what it replaced, through
// the Task Writer as the person undoing it (so they need to be able to make
// that change by hand), newest first, and only where the task still holds
// what the rule set: a change someone made since is theirs and stays.
// Comments and notices stay; a subtask the run added is deleted when the
// person may delete it.

import (
	"context"
	"encoding/json"
	"errors"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// ErrNotUndoable is a run with nothing to undo, or one undone already.
var ErrNotUndoable = errors.New("that run can't be undone")

// Seams for tests.
var (
	getRun     = workflowModel.GetRun
	markUndone = workflowModel.MarkUndone
)

// UndoRun undoes one of a project's runs as who, and answers the run as it
// now stands.
func UndoRun(ctx context.Context, who *userModels.UserInfo, projectID, runID uuid.UUID) (*workflowModel.Run, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	run, err := getRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.ProjectID == nil || *run.ProjectID != projectID {
		return nil, errNotThisProject
	}
	if run.Status != workflowModel.RunOK && run.Status != workflowModel.RunFailed {
		return nil, ErrNotUndoable
	}
	steps := make([]workflowModel.Step, len(run.Steps))
	copy(steps, run.Steps)
	undid := 0
	for i := len(steps) - 1; i >= 0; i-- {
		st := &steps[i]
		if st.Status != "ok" {
			continue
		}
		if done, detail := undoStep(ctx, who, st); done {
			st.Status, st.Detail = "undone", detail
			undid++
		} else if detail != "" {
			st.Detail = st.Detail + " · " + detail
		}
	}
	if undid == 0 {
		return nil, ErrNotUndoable
	}
	ok, err := markUndone(ctx, runID, who.UserPostgresInfo.Id, steps)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotUndoable
	}
	return getRun(ctx, runID)
}

// undoStep puts one step back; it answers whether it did, and why not.
func undoStep(ctx context.Context, who *userModels.UserInfo, st *workflowModel.Step) (bool, string) {
	taskID, err := uuid.Parse(st.TaskID)
	if err != nil {
		return false, ""
	}
	if st.Type == ActionAddSubtask && st.Made != "" {
		sub, err := uuid.Parse(st.Made)
		if err != nil {
			return false, ""
		}
		if err := applyOps(ctx, who, sub, []taskWriter.Op{{Kind: taskWriter.OpDelete}}); err != nil {
			return false, "kept the subtask: " + plainError(err)
		}
		return true, "subtask deleted"
	}
	if st.Field == "" {
		return false, ""
	}
	task, err := readTask(ctx, st.TaskID, who)
	if err != nil || task == nil {
		return false, "you can't see the task"
	}
	var before, after string
	_ = json.Unmarshal(st.Before, &before)
	_ = json.Unmarshal(st.After, &after)
	var now string
	var op taskWriter.Op
	switch st.Field {
	case "status":
		now = taskStatusBusiness.Of(task).Value()
		op = taskWriter.Op{Kind: taskWriter.OpStatus, Value: raw(before)}
	case "priority":
		now = task.Priority
		op = taskWriter.Op{Kind: taskWriter.OpPriority, Value: raw(before)}
	case "assignee":
		if task.Assignee != nil {
			now = task.Assignee.Uuid
		}
		op = taskWriter.Op{Kind: taskWriter.OpAssignee, Value: raw(before)}
	case "due":
		now = wireTime(task.DueDate)
		var v any
		if before != "" {
			v = before
		}
		op = taskWriter.Op{Kind: taskWriter.OpDue, Value: raw(v)}
	default:
		return false, ""
	}
	if now != after {
		return false, "changed since, kept"
	}
	if st.Field == "priority" && before == "" {
		return false, "had no priority, kept"
	}
	if err := applyOps(helpers.WithWorkflowGenerated(ctx), who, taskID, []taskWriter.Op{op}); err != nil {
		return false, plainError(err)
	}
	return true, "put back"
}
