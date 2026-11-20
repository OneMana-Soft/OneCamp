//go:build integration

package business

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Arrival rules are a project's rules at every door: the workspace's rule
// door (POST and PUT /workflows) refuses a rule on arriving work, a triage
// action and a test of the door alike, in words the user meets, and keeps
// nothing it refused.
func TestTheWorkspaceRuleDoorRefusesArrivalRules(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	_ = integration.SetupDgraph(t)
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, owner, owner.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Fresh rows for every door: checking a rule trims its conditions in place.
	rows := func() map[string]WorkflowInput {
		triage := []WorkflowAction{{Type: ActionTriage, Decision: "accept"}}
		fromAForm := Conditions{Items: []Condition{{Field: "source", Op: "is", Value: "form"}}}
		refused := map[string]WorkflowInput{
			"(A) a workspace rule on arriving work": {Name: "refused", IsActive: true,
				TriggerType: workflowModel.TriggerTaskArrived, Actions: triage},
			"(B) a workspace task-created rule that triages": {Name: "refused", IsActive: true,
				TriggerType: workflowModel.TriggerTaskCreated, Actions: triage},
			"(B) an otherwise branch that triages": {Name: "refused", IsActive: true,
				TriggerType: workflowModel.TriggerTaskCreated, Conditions: Conditions{Items: []Condition{{Field: "priority", Op: "is", Value: "high"}}},
				Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}, ElseActions: triage},
			"(C) a workspace rule that tests the door": {Name: "refused", IsActive: true,
				TriggerType: workflowModel.TriggerTaskCreated, Conditions: fromAForm,
				Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}},
		}
		// A padded field is trimmed when the conditions are checked: the door test
		// is refused all the same, never saved as a real "source".
		for _, field := range []string{" source", "source "} {
			refused["(C) a padded field "+strconv.Quote(field)] = WorkflowInput{Name: "refused", IsActive: true,
				TriggerType: workflowModel.TriggerTaskCreated, Conditions: Conditions{Items: []Condition{{Field: field, Op: "is", Value: "form"}}},
				Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}
		}
		return refused
	}
	for name, in := range rows() {
		if _, err := CreateWorkflow(ctx, in, owner); err == nil || !strings.Contains(err.Error(), "arriving work") {
			t.Errorf("create, %s: %v", name, err)
		}
	}
	plain, err := CreateWorkflow(ctx, WorkflowInput{Name: "plain", IsActive: true, TriggerType: workflowModel.TriggerTaskCreated,
		Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}, owner)
	if err != nil {
		t.Fatalf("a plain workspace task rule was refused: %v", err)
	}
	for name, in := range rows() {
		if _, err := UpdateWorkflow(ctx, plain.Id, in, Actor{UserID: owner}); err == nil || !strings.Contains(err.Error(), "arriving work") {
			t.Errorf("update, %s: %v", name, err)
		}
	}
	// The project's door refuses the padded field too.
	who := &userModels.UserInfo{}
	who.UserPostgresInfo.Id = owner
	padded := rows()[`(C) a padded field "source "`]
	if _, err := CreateProjectRule(ctx, who, uuid.New(), padded); err == nil || !strings.Contains(err.Error(), "arriving work") {
		t.Errorf("project door, a padded field: %v", err)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM workflows WHERE created_by = $1 AND (name = 'refused' OR trigger_type = 'task_arrived')`, owner).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("%d refused rules were kept", kept)
	}
}
