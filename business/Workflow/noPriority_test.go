package business

import (
	"context"
	"testing"

	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// UX-PROJECTS-H1-BE (PROJECTS-14): a task made without a priority has none,
// and a rule's is no different. Its create task step with no priority made
// the task medium.
func TestARulesTaskWithoutAPriorityHasNone(t *testing.T) {
	f := newStore(t)
	pid := f.task.Project.Uuid
	oldMake := makeTask
	t.Cleanup(func() { makeTask = oldMake })
	got := "unset"
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		got = in.Priority
		return uuid.New(), nil
	}
	cw := rule(Conditions{}, []WorkflowAction{{Type: ActionCreateTask, ProjectID: pid, TaskName: "Follow up"}}, nil)
	runWorkflow(context.Background(), cw, f.event("e1"))
	if got != "" {
		t.Fatalf("the rule's task was made %q, want no priority", got)
	}
}

// UX-PROJECTS-H1-BE: undo puts back No priority. Tasks are made without one
// now, so a rule that set a new task's priority could never be undone: undo
// kept the rule's priority, saying the task "had no priority".
func TestUndoPutsBackNoPriority(t *testing.T) {
	f := newStore(t)
	f.task.Priority = ""
	cw := rule(Conditions{}, []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, nil)
	runWorkflow(context.Background(), cw, f.event("e1"))
	run := f.runs["e1"]
	run.ProjectID = ptr(uuid.MustParse(f.task.Project.Uuid))
	oldGet, oldMark := getRun, markUndone
	defer func() { getRun, markUndone = oldGet, oldMark }()
	getRun = func(context.Context, uuid.UUID) (*workflowModel.Run, error) { return run, nil }
	var marked []workflowModel.Step
	markUndone = func(_ context.Context, _, _ uuid.UUID, steps []workflowModel.Step) (bool, error) {
		marked = steps
		return true, nil
	}
	f.ops = nil
	undoer := &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: "undoer"}}
	if _, err := UndoRun(context.Background(), undoer, *run.ProjectID, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != 1 || f.ops[0] != `undoer priority ""` || f.task.Priority != "" {
		t.Fatalf("undo made %v, priority %q", f.ops, f.task.Priority)
	}
	if len(marked) != 1 || marked[0].Status != "undone" {
		t.Errorf("steps %+v", marked)
	}
}
