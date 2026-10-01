package business

import (
	"context"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestEventAgentsNarrowToTheMoveTheyName(t *testing.T) {
	proj := uuid.NewString()
	agent := &model.AiAgent{TriggerConfig: `{"event":"task.status_changed","project_id":"` + proj + `","to_status":"done"}`}
	into := map[string]interface{}{"project_id": proj, "old_status": "inReview", "new_status": "done"}
	elsewhere := map[string]interface{}{"project_id": uuid.NewString(), "old_status": "inReview", "new_status": "done"}
	notDone := map[string]interface{}{"project_id": proj, "old_status": "todo", "new_status": "inProgress"}
	if !eventWanted(agent, "task.status_changed", into) {
		t.Error("a move into Done in its project did not launch it")
	}
	if eventWanted(agent, "task.status_changed", elsewhere) || eventWanted(agent, "task.status_changed", notDone) {
		t.Error("a move it did not ask for launched it")
	}
	// Unnarrowed agents, and every other event, run as before.
	if !eventWanted(&model.AiAgent{TriggerConfig: `{"event":"task.status_changed"}`}, "task.status_changed", notDone) {
		t.Error("an unnarrowed agent was filtered")
	}
	if !eventWanted(agent, "task.created", map[string]interface{}{}) {
		t.Error("another event was filtered")
	}
}

func TestNormalizeAgentMoveFilter(t *testing.T) {
	ctx := context.Background()
	in := AgentInput{TriggerType: model.TriggerEvent, TriggerConfig: map[string]interface{}{"event": "task.status_changed", "to_status": "Done"}}
	if err := normalizeAgentMoveFilter(ctx, &in); err != nil {
		t.Fatal(err)
	}
	if in.TriggerConfig["to_status"] != "done" || in.TriggerConfig["event"] != "task.status_changed" {
		t.Fatalf("%v", in.TriggerConfig)
	}
	bad := AgentInput{TriggerType: model.TriggerEvent, TriggerConfig: map[string]interface{}{"event": "task.status_changed", "to_status": "QA"}}
	if err := normalizeAgentMoveFilter(ctx, &bad); err == nil {
		t.Fatal("a project's own status with no project was accepted")
	}
	// A filter left behind by an earlier choice goes with it.
	other := AgentInput{TriggerType: model.TriggerEvent, TriggerConfig: map[string]interface{}{"event": "task.created", "to_status": "done", "project_id": "x"}}
	if err := normalizeAgentMoveFilter(ctx, &other); err != nil || len(other.TriggerConfig) != 1 {
		t.Fatalf("%v %v", other.TriggerConfig, err)
	}
}
