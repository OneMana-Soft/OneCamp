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
