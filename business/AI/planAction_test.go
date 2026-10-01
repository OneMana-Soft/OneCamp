package business

import (
	"strings"
	"testing"
)

func TestBuildPlanSummary(t *testing.T) {
	// Singular phrasing + numbered, description-or-toolname fallback.
	one := BuildPlanSummary("Standup Bot", []PlanStep{
		{ToolName: "create_task", Description: `Create task "Fix login"`},
	})
	if !strings.Contains(one, "1 step") || !strings.Contains(one, "1. Create task") {
		t.Fatalf("unexpected singular summary: %q", one)
	}

	many := BuildPlanSummary("", []PlanStep{
		{ToolName: "create_task", Description: "Create the task"},
		{ToolName: "send_message"}, // no description -> tool name
	})
	if !strings.Contains(many, "Agent proposes a 2-step plan") {
		t.Fatalf("expected default name + count, got %q", many)
	}
	if !strings.Contains(many, "1. Create the task") || !strings.Contains(many, "2. send_message") {
		t.Fatalf("expected numbered steps with toolname fallback, got %q", many)
	}
}

func TestPlanStepsRoundTrip(t *testing.T) {
	steps := []PlanStep{
		{ToolName: "create_task", Params: map[string]string{"task_name": "X", "project_uuid": "p"}, Description: "Create X"},
		{ToolName: "send_message", Params: map[string]string{"channel_uuid": "c", "text": "done"}},
	}
	enc, err := encodePlanSteps(steps)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodePlanSteps(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || got[0].ToolName != "create_task" || got[0].Params["task_name"] != "X" || got[1].ToolName != "send_message" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Empty/blank decodes to nothing (no panic).
	if s, err := decodePlanSteps(""); err != nil || len(s) != 0 {
		t.Fatalf("empty decode should yield no steps, got %+v err %v", s, err)
	}
}
