package business

import (
	"context"
	"strings"
	"testing"

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
