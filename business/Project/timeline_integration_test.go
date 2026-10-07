//go:build integration

package business

// A project's timeline against a real Dgraph with the production schema.
// Run: go test -tags=integration ./business/Project/ -run TestProjectTimeline -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestProjectTimeline(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	project := uuid.NewString()
	live, gone := "0001-01-01T00:00:00Z", "2026-03-10T00:00:00Z"
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:p", "dgraph.type": "Project", "project_uuid": project,
		"project_admins": []map[string]any{{"uid": "_:admin", "dgraph.type": "User", "user_uuid": uuid.NewString()}},
		"project_tasks": []map[string]any{
			{"uid": "_:a", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Plan", "task_deleted_at": live,
				"task_created_at": "2026-03-01T09:00:00Z", "task_start_date": "2026-03-02T09:00:00Z", "task_due_date": "2026-03-04T17:00:00Z",
				"task_sub_tasks": []map[string]any{{"uid": "_:s", "dgraph.type": "Task", "task_name": "step", "task_deleted_at": live, "task_parent_task": map[string]any{"uid": "_:a"}}}},
			{"uid": "_:b", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Build", "task_deleted_at": live, "task_created_at": "2026-03-02T09:00:00Z"},
			{"uid": "_:c", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_name": "Deleted", "task_deleted_at": gone, "task_created_at": "2026-03-03T09:00:00Z"},
		},
		"project_members": []map[string]any{{"uid": "_:member", "dgraph.type": "User", "user_uuid": uuid.NewString()}},
	})
	dg.Mutate(t, map[string]any{"uid": uids["p"], "project_tasks": []map[string]any{{"uid": uids["s"]}}})

	p, err := GetProjectTimeline(ctx, project, uids["admin"])
	if err != nil {
		t.Fatal(err)
	}
	if p.TaskCount != 2 || len(p.Tasks) != 2 || p.Tasks[0].Name != "Build" || p.Tasks[1].Name != "Plan" {
		t.Fatalf("live top-level tasks, newest first: count %d, %+v", p.TaskCount, p.Tasks)
	}
	if p.IsProjectAdmin != 1 || p.Tasks[1].SubTaskCount != 1 || p.Tasks[1].StartDate == nil || p.Tasks[1].DueDate == nil {
		t.Fatalf("admin flag, subtask count and dates: %d %+v", p.IsProjectAdmin, p.Tasks[1])
	}
	if p.Tasks[1].Description != nil {
		t.Fatal("the timeline leaves descriptions out")
	}
	other, err := GetProjectTimeline(ctx, project, uids["member"])
	if err != nil || other.IsProjectAdmin != 0 {
		t.Fatalf("a member may read, not edit: %v %d", err, other.IsProjectAdmin)
	}
}
