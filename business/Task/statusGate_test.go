package business

import (
	"context"
	"errors"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Every status change asks the gate first (status rules and approvals,
// business/Approvals), with where the task is and where it goes, and a gate
// that says no changes nothing. A move to where it already is asks nothing.
func TestAStatusChangeAsksTheGateFirst(t *testing.T) {
	old := StatusGate
	t.Cleanup(func() { StatusGate = old })
	refused := errors.New("only the project's admins move tasks to Done")
	var asked []string
	StatusGate = func(_ context.Context, task *dgraphStruct.DgraphTask, prev, next taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error {
		asked = append(asked, prev.Category+">"+next.Category+" by "+by.Uuid)
		return refused
	}
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: dgraphStruct.TASK_STATUS_TODO}
	who := &dgraphStruct.DgraphUser{Uuid: "ana", Uid: "0x1"}
	if err := UpdateTaskStatusByTaskUUID(context.Background(), uuid.New(), dgraphStruct.TASK_STATUS_DONE, task, who); !errors.Is(err, refused) {
		t.Fatalf("the gate's refusal: %v", err)
	}
	if len(asked) != 1 || asked[0] != "todo>done by ana" {
		t.Errorf("asked %v", asked)
	}
	if err := UpdateTaskStatusByTaskUUID(context.Background(), uuid.New(), dgraphStruct.TASK_STATUS_TODO, task, who); err != nil || len(asked) != 1 {
		t.Errorf("a move to where it is: %v, asked %v", err, asked)
	}
}

// A move's note says the gate found no open request only for its own task
// and only once the gate noted it (H4W): WentOn closes as before wherever
// nothing was noted, the task had one open, or another task is asked about.
func TestAMoveNoteSpeaksOnlyForItsOwnTask(t *testing.T) {
	task, other := uuid.New(), uuid.New()
	if NoOpenRequest(context.Background(), task) {
		t.Fatal("a context with no move says no request is open")
	}
	ctx := withMoveNote(context.Background(), task)
	if NoOpenRequest(ctx, task) {
		t.Fatal("a move the gate noted nothing for says no request is open")
	}
	NoteOpenRequest(ctx, other, false)
	if NoOpenRequest(ctx, task) || NoOpenRequest(ctx, other) {
		t.Fatal("a note about another task spoke for the move")
	}
	NoteOpenRequest(ctx, task, true)
	if NoOpenRequest(ctx, task) {
		t.Fatal("a task with an open request says none is open")
	}
	NoteOpenRequest(ctx, task, false)
	if !NoOpenRequest(ctx, task) {
		t.Fatal("a task the gate found with no open request should say so")
	}
}
