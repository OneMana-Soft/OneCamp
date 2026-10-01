package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestScoreRun_AllExpectationTypes(t *testing.T) {
	outcome := &RunOutcome{
		Status:    model.RunSucceeded,
		Result:    "I created the task and notified the team.",
		ToolsUsed: []string{"create_task", "send_message"},
	}
	exp := Expectations{
		MustContain:    []string{"created the task"},
		MustNotContain: []string{"error"},
		ExpectedTools:  []string{"create_task"},
		ForbiddenTools: []string{"delete_task"},
		ExpectedStatus: model.RunSucceeded,
	}
	res := ScoreRun(outcome, exp)
	if res.Inconclusive {
		t.Fatalf("expected conclusive, got inconclusive: %s", res.Reason)
	}
	if !res.Passed || res.Score != 100 {
		t.Fatalf("expected full pass, got passed=%v score=%d checks=%+v", res.Passed, res.Score, res.Checks)
	}
}

func TestScoreRun_PartialAndFail(t *testing.T) {
	outcome := &RunOutcome{
		Status:    model.RunSucceeded,
		Result:    "Done.",
		ToolsUsed: []string{"list_tasks"},
	}
	// Two checks: one passes (status), one fails (expected tool not used).
	exp := Expectations{
		ExpectedTools:  []string{"create_task"},
		ExpectedStatus: model.RunSucceeded,
	}
	res := ScoreRun(outcome, exp)
	if res.Passed {
		t.Fatal("expected overall fail when a tool expectation is unmet")
	}
	if res.Score != 50 {
		t.Fatalf("expected 50%% (1 of 2), got %d", res.Score)
	}

	// Forbidden tool actually used -> that check fails.
	res2 := ScoreRun(&RunOutcome{Status: model.RunSucceeded, ToolsUsed: []string{"delete_task"}},
		Expectations{ForbiddenTools: []string{"delete_task"}})
	if res2.Passed {
		t.Fatal("expected fail when a forbidden tool is used")
	}

	// must_not_contain violated.
	res3 := ScoreRun(&RunOutcome{Status: model.RunSucceeded, Result: "An ERROR occurred"},
		Expectations{MustNotContain: []string{"error"}})
	if res3.Passed {
		t.Fatal("expected fail when answer contains a forbidden phrase (case-insensitive)")
	}
}

func TestScoreRun_SmokeTestWhenNoExpectations(t *testing.T) {
	if res := ScoreRun(&RunOutcome{Status: model.RunSucceeded}, Expectations{}); !res.Passed || res.Score != 100 {
		t.Fatalf("a clean run with no expectations should pass as a smoke test, got %+v", res)
	}
	if res := ScoreRun(&RunOutcome{Status: model.RunFailed, Error: "the AI model call failed"}, Expectations{}); res.Passed {
		t.Fatal("a failed run with no expectations should not pass the smoke test")
	}
}

func TestScoreRun_Inconclusive(t *testing.T) {
	for _, reason := range []string{
		"AI temporarily unavailable (circuit open)",
		"rate limit reached",
		"the workspace AI token budget for today has been reached",
	} {
		res := ScoreRun(&RunOutcome{Status: model.RunStopped, Error: reason}, Expectations{MustContain: []string{"x"}})
		if !res.Inconclusive {
			t.Fatalf("expected inconclusive for %q, got %+v", reason, res)
		}
		if res.Passed {
			t.Fatalf("inconclusive must not count as passed for %q", reason)
		}
	}
}
