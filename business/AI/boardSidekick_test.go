package business

import "testing"

func TestParseSidekickPlan_PlanReady(t *testing.T) {
	raw := `{"title":"Onboarding flow","ready":true,"questions":[],"steps":["Signup","Verify email","First value","Activation"],"suggested_type":"flow"}`
	p, err := parseSidekickPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.Ready {
		t.Fatal("expected ready=true when steps present")
	}
	if len(p.Steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(p.Steps))
	}
	if len(p.Questions) != 0 {
		t.Fatalf("questions must be dropped when planning, got %d", len(p.Questions))
	}
	if p.SuggestedType != "flow" {
		t.Fatalf("expected suggested_type flow, got %q", p.SuggestedType)
	}
}

func TestParseSidekickPlan_ClarifyWhenNoSteps(t *testing.T) {
	raw := `{"title":"","ready":true,"questions":["What product is this for?","Who are the users?"],"steps":[],"suggested_type":"flow"}`
	p, err := parseSidekickPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Even though the model said ready=true, no steps ⇒ must be treated as
	// needing clarification.
	if p.Ready {
		t.Fatal("expected ready=false when no steps")
	}
	if len(p.Questions) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(p.Questions))
	}
}

func TestParseSidekickPlan_StepsWinOverQuestions(t *testing.T) {
	// A confused model returned BOTH; steps must win and questions be cleared.
	raw := `{"ready":false,"questions":["really?"],"steps":["A","B","C"],"suggested_type":"mindmap"}`
	p, err := parseSidekickPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.Ready || len(p.Steps) != 3 || len(p.Questions) != 0 {
		t.Fatalf("steps should win: %+v", p)
	}
}

func TestParseSidekickPlan_StripsListMarkersAndCaps(t *testing.T) {
	raw := `{"ready":true,"steps":["1. First step","- Second step","• Third"],"suggested_type":"flow"}`
	p, err := parseSidekickPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"First step", "Second step", "Third"}
	for i, w := range want {
		if p.Steps[i] != w {
			t.Fatalf("step %d = %q, want %q", i, p.Steps[i], w)
		}
	}
}

func TestParseSidekickPlan_EmptyIsError(t *testing.T) {
	if _, err := parseSidekickPlan(`{"ready":true,"questions":[],"steps":[]}`); err == nil {
		t.Fatal("expected error when neither steps nor questions produced")
	}
	if _, err := parseSidekickPlan(`not json`); err == nil {
		t.Fatal("expected error for non-JSON")
	}
}

func TestNormalizeSuggestedType(t *testing.T) {
	if got := normalizeSuggestedType("MINDMAP"); got != "mindmap" {
		t.Fatalf("got %q", got)
	}
	if got := normalizeSuggestedType("nonsense"); got != BoardDiagramFlow {
		t.Fatalf("expected fallback to flow, got %q", got)
	}
}
