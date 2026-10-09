//go:build integration

package business

import (
	"context"
	"strings"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Saving a "when a task moves to QA" workflow stores QA's id, so renaming QA
// later cannot break it, and a status the project lacks is refused by name.
func TestTaskStatusTriggerResolvesTheProjectsOwnStatus(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project := uuid.New()
	qa, err := taskStatusBusiness.Create(ctx, project, uuid.New(), taskStatusBusiness.Input{Name: "QA", Category: "inReview"})
	if err != nil {
		t.Fatal(err)
	}
	in := func(status string) WorkflowInput {
		return WorkflowInput{
			TriggerType:   workflowModel.TriggerTaskStatusChanged,
			TriggerConfig: map[string]interface{}{"project_id": project.String(), "to_status": status},
			ChannelID:     uuid.NewString(),
			Actions:       []WorkflowAction{{Type: ActionReply, Text: "{task} is ready for {status}"}},
		}
	}
	w := in("qa")
	if err := normalizeTaskStatusTrigger(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if w.TriggerConfig["to_status"] != qa.ID.String() || w.TriggerConfig["project_id"] != project.String() {
		t.Fatalf("config %v", w.TriggerConfig)
	}
	w = in("Shipped")
	err = normalizeTaskStatusTrigger(ctx, &w)
	if err == nil || !strings.Contains(err.Error(), "Shipped") || !strings.Contains(err.Error(), "QA") {
		t.Fatalf("an unknown status should be refused with the ones there are: %v", err)
	}
}

// The whole path: a saved workflow is loaded, a move into its status runs it,
// and a move elsewhere does not. Its action is create_task, whose executor is
// not registered in this test binary, so a run records that error rather than
// reaching the bot and search, which the test has no server for.
func TestTaskStatusWorkflowRunsOnTheMoveItNames(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	user, project := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// A workflow hears moves only in a project its owner is in (access.go).
	dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "user_uuid": user.String(), "user_name": "maya"},
		{"uid": "_:p", "project_uuid": project.String(), "project_name": "Q4 launch",
			"project_members": []map[string]any{{"uid": "_:u"}}},
	})
	qa, err := taskStatusBusiness.Create(ctx, project, user, taskStatusBusiness.Input{Name: "QA", Category: "inReview"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateWorkflow(ctx, WorkflowInput{
		Name:          "QA handoff",
		IsActive:      true,
		TriggerType:   workflowModel.TriggerTaskStatusChanged,
		TriggerConfig: map[string]interface{}{"project_id": project.String(), "to_status": "QA"},
		Actions:       []WorkflowAction{{Type: ActionCreateTask, ProjectID: project.String(), TaskName: "Test {task}"}},
	}, user)
	if err != nil {
		t.Fatal(err)
	}
	runs := func() int64 {
		t.Helper()
		got, err := workflowModel.GetWorkflowByID(ctx, w.Id)
		if err != nil {
			t.Fatal(err)
		}
		return got.RunCount
	}
	move := func(newStatus, newCustom string) {
		handleEvent(ctx, "task.status_changed", map[string]interface{}{
			"task_id": uuid.NewString(), "task_name": "Set up SSO", "project_id": project.String(),
			"old_status": "inProgress", "new_status": newStatus, "old_custom_id": "", "new_custom_id": newCustom,
			"old_status_name": "In Progress", "new_status_name": "QA", "updated_by_name": "Maya", "updated_by_uuid": user.String(),
		})
	}
	move("inReview", "") // In Review, not QA
	if n := runs(); n != 0 {
		t.Fatalf("a move into In Review ran the QA workflow (%d runs)", n)
	}
	move("inReview", qa.ID.String())
	if n := runs(); n != 1 {
		t.Fatalf("a move into QA ran it %d times", n)
	}
}
