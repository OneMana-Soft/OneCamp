package business

import (
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// proposalDescription must read as "<Agent>: <what it wants to do>", falling
// back to the tool name when the model gave no description, so the approval
// card is self-explanatory.
func TestProposalDescription(t *testing.T) {
	agent := &model.AiAgent{Name: "Standup Bot"}

	got := proposalDescription(agent, ai.ProposedAction{ToolName: "create_task", Description: "Open a task for the launch blocker"})
	if got != "Standup Bot: Open a task for the launch blocker" {
		t.Fatalf("unexpected description: %q", got)
	}

	// No description -> falls back to the tool name.
	got = proposalDescription(agent, ai.ProposedAction{ToolName: "create_task"})
	if !strings.Contains(got, "create_task") || !strings.HasPrefix(got, "Standup Bot: ") {
		t.Fatalf("fallback should use the tool name: %q", got)
	}

	// Unnamed agent still produces a sensible label.
	got = proposalDescription(&model.AiAgent{}, ai.ProposedAction{ToolName: "send_message"})
	if !strings.HasPrefix(got, "Agent: ") {
		t.Fatalf("unnamed agent should fall back to 'Agent': %q", got)
	}
}

// ValidAutonomy gates the persisted mode so a bad value can never be stored.
func TestValidAutonomy(t *testing.T) {
	for _, ok := range []string{model.AutonomyAuto, model.AutonomyApproval} {
		if !model.ValidAutonomy(ok) {
			t.Fatalf("expected %q to be valid", ok)
		}
	}
	for _, bad := range []string{"", "AUTO", "yolo", "autonomous"} {
		if model.ValidAutonomy(bad) {
			t.Fatalf("expected %q to be invalid", bad)
		}
	}
}
