package business

import (
	"strings"
	"testing"
)

func TestParseAgentDraft(t *testing.T) {
	raw := "```json\n{\"name\":\"Standup Bot\",\"description\":\"Daily standup\",\"instructions\":\"Ask for updates\",\"enabled_tools\":[\"send_message\"],\"trigger_type\":\"schedule\",\"autonomy\":\"approval\",\"max_steps\":6,\"dm_able\":true}\n```"
	d, err := parseAgentDraft(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Name != "Standup Bot" || d.TriggerType != "schedule" || d.MaxSteps != 6 || !d.DmAble {
		t.Fatalf("unexpected parse: %+v", d)
	}
}

func TestParseAgentDraftBad(t *testing.T) {
	if _, err := parseAgentDraft("not json"); err == nil {
		t.Fatal("expected error for non-JSON")
	}
}

func TestSanitizeAgentDraftClampsVocabulary(t *testing.T) {
	in := &AgentDraft{
		Name:         "",
		Instructions: "  do things  ",
		EnabledTools: []string{"send_message", "not_a_real_tool", "send_message", "create_task"},
		TriggerType:  "weird",
		Autonomy:     "yolo",
		MaxSteps:     999,
	}
	out := sanitizeAgentDraft(in)

	if out.Name != "New agent" {
		t.Fatalf("expected default name, got %q", out.Name)
	}
	if out.Instructions != "do things" {
		t.Fatalf("instructions not trimmed: %q", out.Instructions)
	}
	// Unknown tool dropped; duplicate de-duped; known tools kept in order.
	want := []string{"send_message", "create_task"}
	if len(out.EnabledTools) != len(want) {
		t.Fatalf("tools = %v, want %v", out.EnabledTools, want)
	}
	for i, w := range want {
		if out.EnabledTools[i] != w {
			t.Fatalf("tool[%d] = %q, want %q", i, out.EnabledTools[i], w)
		}
	}
	if out.TriggerType != "manual" {
		t.Fatalf("invalid trigger should fall back to manual, got %q", out.TriggerType)
	}
	// Changed on purpose (ADMIN-16): an unreadable choice asks first.
	if out.Autonomy != "approval" {
		t.Fatalf("invalid autonomy should fall back to approval, got %q", out.Autonomy)
	}
	if out.MaxSteps != draftDefaultMaxSteps {
		t.Fatalf("out-of-range max_steps should default, got %d", out.MaxSteps)
	}
}

func TestSanitizeAgentDraftCapsName(t *testing.T) {
	long := strings.Repeat("x", draftMaxName+50)
	out := sanitizeAgentDraft(&AgentDraft{Name: long, TriggerType: "manual", Autonomy: "auto", MaxSteps: 5})
	if len(out.Name) > draftMaxName {
		t.Fatalf("name not capped: %d", len(out.Name))
	}
}

func TestSanitizeAgentDraftValidPassThrough(t *testing.T) {
	out := sanitizeAgentDraft(&AgentDraft{
		Name: "Triage", Instructions: "Triage incoming requests",
		EnabledTools: []string{"list_tasks"}, TriggerType: "mention", Autonomy: "approval", MaxSteps: 10,
	})
	if out.TriggerType != "mention" || out.Autonomy != "approval" || out.MaxSteps != 10 {
		t.Fatalf("valid values should pass through: %+v", out)
	}
}

// A draft that names no autonomy, or one that isn't, asks first: a new agent
// waits for a yes until someone chooses otherwise (ADMIN-16).
func TestDraftFallbackIsApproval(t *testing.T) {
	for _, a := range []string{"", "  ", "yolo", "AUTONOMOUS"} {
		if got := sanitizeAgentDraft(&AgentDraft{Autonomy: a}).Autonomy; got != "approval" {
			t.Errorf("draft autonomy %q became %q, want approval", a, got)
		}
	}
	for _, a := range []string{"auto", "Plan", "approval"} {
		if got := sanitizeAgentDraft(&AgentDraft{Autonomy: a}).Autonomy; got != strings.ToLower(a) {
			t.Errorf("a chosen autonomy %q is kept: got %q", a, got)
		}
	}
}
