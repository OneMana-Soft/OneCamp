package Domain

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// TaskAssignee is a task and whom it is given to now: a user uuid, or "" for
// nobody.
type TaskAssignee struct {
	Task     string
	Assignee string
}

// TaskAssigneePage reads a page of every task, trashed ones too, with its
// assignee now: first of them after offset, in the graph's order. For a pass
// over every task's AI entry (business/Task assigneeAIBackfill.go).
func TaskAssigneePage(ctx context.Context, offset, first int) ([]TaskAssignee, error) {
	rows, err := dgexec.List[dgraphStruct.DgraphTask](ctx, fmt.Sprintf(`{
		page(func: has(task_uuid), first: %d, offset: %d) {
			task_uuid
			task_assignee { user_uuid }
		}
	}`, first, offset), nil, "page")
	if err != nil {
		return nil, err
	}
	out := make([]TaskAssignee, 0, len(rows))
	for _, t := range rows {
		if t.Uuid != "" {
			out = append(out, TaskAssignee{Task: t.Uuid, Assignee: assigneeUUID(t)})
		}
	}
	return out, nil
}

// TaskAssignees reads the named tasks' assignees now (task uuid -> user uuid,
// "" for nobody), trashed tasks too, in one read: a task the graph doesn't
// hold isn't in it.
func TaskAssignees(ctx context.Context, taskUUIDs []string) (map[string]string, error) {
	out := map[string]string{}
	ids := uuidsIn(taskUUIDs)
	if len(ids) == 0 {
		return out, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := dgexec.List[dgraphStruct.DgraphTask](ctx, `{
		tasks(func: eq(task_uuid, `+string(list)+`)) {
			task_uuid
			task_assignee { user_uuid }
		}
	}`, nil, "tasks")
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		if t.Uuid != "" {
			out[t.Uuid] = assigneeUUID(t)
		}
	}
	return out, nil
}

func assigneeUUID(t dgraphStruct.DgraphTask) string {
	if t.Assignee == nil {
		return ""
	}
	return t.Assignee.Uuid
}
