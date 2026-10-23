//go:build integration

package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A read with nobody asking (a system read: an agent's work on a task, a
// thread's task) finds the task and its project. Dgraph refused it while the
// read counted an empty asker's standing, so those callers never found one.
// Run: PKGS=./business/Task/ integ_rel.sh <wt> <log> 'TestASystemReadFindsTheTask'
func TestASystemReadFindsTheTask(t *testing.T) {
	integration.ConnectServerPostgres(t)
	dg := integration.SetupDgraph(t)
	const live = "0001-01-01T00:00:00Z"
	task, project := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": project, "project_name": "Payroll", "project_deleted_at": live},
		{"uid": "_:t", "dgraph.type": "Task", "task_uuid": task, "task_name": "Send the invoices", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:P"}},
	})
	got, err := GetDgraphTaskInfo(helpers.WithSystemRead(context.Background()), task, "")
	if err != nil || got == nil || got.Project == nil || got.Project.Uuid != project {
		t.Fatalf("a system read of the task: %+v, %v; want it, in its project", got, err)
	}
}
