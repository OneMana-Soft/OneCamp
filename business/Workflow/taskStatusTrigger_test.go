package business

import (
	"context"
	"strings"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// Built-in statuses resolve without the database; a project's own are in the
// integration test.
func TestNormalizeTaskStatusTrigger(t *testing.T) {
	ctx := context.Background()
	in := WorkflowInput{
		TriggerType:   workflowModel.TriggerTaskStatusChanged,
		TriggerConfig: map[string]interface{}{"to_status": "In review", "junk": 1},
		ChannelID:     uuid.NewString(),
		Actions:       []WorkflowAction{{Type: ActionReply, Text: "{by} moved {task} to {status}"}},
	}
	if err := normalizeTaskStatusTrigger(ctx, &in); err != nil {
		t.Fatal(err)
	}
	if in.TriggerConfig["to_status"] != "inReview" || len(in.TriggerConfig) != 1 {
		t.Fatalf("config %v", in.TriggerConfig)
	}

	noChannel := in
	noChannel.ChannelID = ""
	noChannel.TriggerConfig = map[string]interface{}{}
	if err := normalizeTaskStatusTrigger(ctx, &noChannel); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("a reply with nowhere to post: %v", err)
	}

	custom := in
	custom.TriggerConfig = map[string]interface{}{"to_status": "QA"}
	if err := normalizeTaskStatusTrigger(ctx, &custom); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("a project's own status with no project: %v", err)
	}

	other := WorkflowInput{TriggerType: workflowModel.TriggerMessagePosted, TriggerConfig: map[string]interface{}{"x": "y"}}
	if err := normalizeTaskStatusTrigger(ctx, &other); err != nil || other.TriggerConfig["x"] != "y" {
		t.Fatalf("another trigger was touched: %v %v", err, other.TriggerConfig)
	}
}

func TestTaskStatusVarsReachTheMessage(t *testing.T) {
	got := applyVars("{by} moved {task} to {status} ({from} before) in {project}", map[string]string{
		"by": "Maya", "task": "Set up SSO", "status": "QA", "from": "In Progress", "project": "Q4 launch",
	})
	if got != "Maya moved Set up SSO to QA (In Progress before) in Q4 launch" {
		t.Fatal(got)
	}
}

// linear-39 round 2: a sub-task's making reaches webhooks as task.created,
// naming its parent (business/Task taskWebhooks.go). A "task is created" rule
// still runs on a top-level task's making only, as it always has: a
// template's sub-tasks would otherwise start it once each.
func TestATaskCreatedRuleRunsOnATopLevelTaskOnly(t *testing.T) {
	cw, err := compile(&workflowModel.Workflow{Id: uuid.New(), Name: "Triage", CreatedBy: uuid.New(), IsActive: true,
		TriggerType: workflowModel.TriggerTaskCreated, TriggerConfig: `{}`, MatchType: "any",
		Actions: `[{"type":"add_comment","text":"x"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if !cw.wants(workflowModel.TriggerTaskCreated, map[string]interface{}{"task_id": "t1", "project_id": "p1"}, taskStatusBusiness.Move{}) {
		t.Error("a top-level task's making didn't start its rule")
	}
	if cw.wants(workflowModel.TriggerTaskCreated, map[string]interface{}{"task_id": "t2", "project_id": "p1", "parent_task_id": "t1"}, taskStatusBusiness.Move{}) {
		t.Error("a sub-task's making started a rule")
	}
}
