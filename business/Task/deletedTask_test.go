package business

import (
	"context"
	"errors"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// A change to a deleted task is refused before anything is written: each of
// these wrote the task's deletion time back to the live value, so a late edit
// (an open tab, an agent, a sync) brought it back in the graph. No store is
// connected here, so a write that got past the check would fail the test.
func TestChangingADeletedTaskIsRefused(t *testing.T) {
	gone := time.Now().Add(-time.Hour)
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), DeletedAt: &gone}
	id := uuid.MustParse(task.Uuid)
	who := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: uuid.NewString()}
	ctx := context.Background()
	for name, change := range map[string]func() error{
		"name":        func() error { return UpdateTaskNameByTaskUUID(ctx, id, "renamed", task, who) },
		"description": func() error { return UpdateTaskDesByTaskUUID(ctx, id, "<p>x</p>", nil, task, who) },
		"assignee":    func() error { return UpdateTaskAssigneeByTaskUUID(ctx, id, who, "", "0x2", task, who) },
		"status":      func() error { return UpdateTaskStatusByTaskUUID(ctx, id, dgraphStruct.TASK_STATUS_DONE, task, who) },
		"priority": func() error {
			return UpdateTaskPriorityByTaskUUID(ctx, id, dgraphStruct.TASK_PRIORITY_MEDIUM, task, who)
		},
		"attachment": func() error { return AddAttachmentToTask(ctx, id, task, &adapter.CreateOrUpdateTaskInput{}, who) },
	} {
		if err := change(); !errors.Is(err, ErrTaskDeleted) {
			t.Errorf("changing the %s of a deleted task: %v, want ErrTaskDeleted", name, err)
		}
	}
}
