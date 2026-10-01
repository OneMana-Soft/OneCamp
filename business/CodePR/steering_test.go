package codepr

import (
	"strings"
	"testing"
)

func TestDistillSteering_TooLittleData(t *testing.T) {
	// Below the minimum sample → no rules, even with a bad merge rate.
	s := Scorecard{Total: 3, Opened: 3, OutcomeKnown: 3, MergeRate: 0.0}
	if got := DistillSteering(s); got != nil {
		t.Fatalf("expected nil below min sample, got %+v", got)
	}
}

func TestDistillSteering_HealthyYieldsNothing(t *testing.T) {
	s := Scorecard{
		Total: 20, Opened: 20, OutcomeKnown: 10,
		MergeRate: 0.9, InScopeRate: 0.95, VerifyRate: 0.95, DraftRate: 0.05,
	}
	if got := DistillSteering(s); got != nil {
		t.Fatalf("healthy scorecard should yield no corrective rules, got %+v", got)
	}
}

func TestDistillSteering_LowMergeRate(t *testing.T) {
	s := Scorecard{Total: 10, Opened: 10, OutcomeKnown: 6, MergeRate: 0.3,
		InScopeRate: 1, VerifyRate: 1, DraftRate: 0}
	got := DistillSteering(s)
	if len(got) != 1 || !strings.Contains(got[0], "closed without merging") {
		t.Fatalf("expected the merge-rejection rule, got %+v", got)
	}
}

func TestDistillSteering_OrdersBySeverityAndCaps(t *testing.T) {
	// All four problems present → ordered (merge > scope > verify > draft), capped.
	s := Scorecard{
		Total: 30, Opened: 30, OutcomeKnown: 10,
		MergeRate: 0.2, InScopeRate: 0.5, VerifyRate: 0.5, DraftRate: 0.9,
	}
	got := DistillSteering(s)
	if len(got) != maxSteeringRules {
		t.Fatalf("expected %d rules, got %d", maxSteeringRules, len(got))
	}
	if !strings.Contains(got[0], "closed without merging") {
		t.Fatalf("merge rule must be first (highest severity): %+v", got)
	}
	if !strings.Contains(got[1], "beyond the task") {
		t.Fatalf("scope rule must be second: %+v", got)
	}
}

func TestDistillSteering_MergeRuleNeedsOutcomeSample(t *testing.T) {
	// Low merge rate but only 2 known outcomes → merge rule suppressed; scope
	// rule (enough opened) still fires.
	s := Scorecard{Total: 10, Opened: 10, OutcomeKnown: 2, MergeRate: 0.0,
		InScopeRate: 0.5, VerifyRate: 1, DraftRate: 0}
	got := DistillSteering(s)
	for _, r := range got {
		if strings.Contains(r, "closed without merging") {
			t.Fatalf("merge rule should be suppressed with too few outcomes: %+v", got)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "beyond the task") {
		t.Fatalf("expected only the scope rule, got %+v", got)
	}
}

func TestFormatSteeringBlock(t *testing.T) {
	if FormatSteeringBlock(nil) != "" {
		t.Fatal("no rules → empty block")
	}
	block := FormatSteeringBlock([]string{"Rule one.", "  ", "Rule two."})
	if !strings.Contains(block, "learned from this repository") {
		t.Fatalf("missing header: %q", block)
	}
	if !strings.Contains(block, "- Rule one.") || !strings.Contains(block, "- Rule two.") {
		t.Fatalf("rules not rendered as bullets: %q", block)
	}
	if strings.Contains(block, "-  \n") {
		t.Fatalf("blank rule should be skipped: %q", block)
	}
}
