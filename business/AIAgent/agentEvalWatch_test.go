package business

import (
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// shouldRerun decides whether to spend model calls on a customer's behalf, so
// every branch is tested. The expensive mistake is rerunning agents nobody
// touched; the useless mistake is never rerunning at all.

func agentAt(updated time.Time) *model.AiAgent {
	return &model.AiAgent{UpdatedAt: updated}
}

func summary(scenarios, passed, scored int, last *time.Time) *model.AgentEvalSummary {
	return &model.AgentEvalSummary{
		ScenarioCount: scenarios, Passed: passed, Scored: scored, LastEvaluatedAt: last,
	}
}

func TestReRunsOnlyAfterAnEditThatHasSettled(t *testing.T) {
	now := time.Now()
	measured := now.Add(-2 * time.Hour)

	// Edited after the measurement, and long enough ago to be finished with.
	edited := now.Add(-evalWatchSettlePeriod - time.Minute)
	if !shouldRerun(agentAt(edited), summary(3, 3, 3, &measured), now) {
		t.Error("did not rerun an agent edited after it was last measured")
	}

	// Edited seconds ago: an owner mid-edit should not be chased by reruns of a
	// half-written instruction.
	if shouldRerun(agentAt(now.Add(-30*time.Second)), summary(3, 3, 3, &measured), now) {
		t.Error("reran an agent that was still being edited")
	}
}

func TestDoesNotReRunWhenTheMeasurementIsCurrent(t *testing.T) {
	now := time.Now()
	measured := now.Add(-time.Minute)
	// Agent last touched BEFORE the measurement: the number on screen is true.
	if shouldRerun(agentAt(now.Add(-time.Hour)), summary(3, 3, 3, &measured), now) {
		t.Error("spent model calls re-measuring an agent nobody had changed")
	}
}

func TestLeavesTheFirstRunToAHuman(t *testing.T) {
	// Scenarios written, never run. The owner may still be drafting them, and
	// spending their token budget uninvited is a bad introduction to the feature.
	now := time.Now()
	if shouldRerun(agentAt(now.Add(-24*time.Hour)), summary(5, 0, 0, nil), now) {
		t.Error("ran a suite that a human had never run once")
	}
}

func TestIgnoresAgentsWithNothingToMeasure(t *testing.T) {
	now := time.Now()
	measured := now.Add(-time.Hour)
	edited := now.Add(-evalWatchSettlePeriod - time.Minute)

	if shouldRerun(agentAt(edited), summary(0, 0, 0, &measured), now) {
		t.Error("ran a suite for an agent with no active scenarios")
	}
	if shouldRerun(nil, summary(3, 3, 3, &measured), now) {
		t.Error("did not tolerate a missing agent")
	}
	if shouldRerun(agentAt(edited), nil, now) {
		t.Error("did not tolerate a missing summary")
	}
}

func TestPassRateSeparatesUnmeasuredFromFailing(t *testing.T) {
	// The distinction the whole regression check rests on: a suite where nothing
	// could be scored is not a suite that scored zero.
	if got := passRate(summary(3, 0, 0, nil)); got != -1 {
		t.Errorf("unmeasured reported as %v, want -1", got)
	}
	if got := passRate(summary(3, 0, 3, nil)); got != 0 {
		t.Errorf("all-failing reported as %v, want 0", got)
	}
	if got := passRate(summary(4, 2, 4, nil)); got != 0.5 {
		t.Errorf("half-passing reported as %v, want 0.5", got)
	}
	if passRate(nil) != -1 {
		t.Error("nil summary should be unmeasured")
	}
}

func TestRegressionOnlyFiresOnARealDrop(t *testing.T) {
	if !regression(1.0, 0.5) {
		t.Error("missed a genuine drop")
	}
	if regression(0.5, 1.0) {
		t.Error("reported an improvement as a regression")
	}
	if regression(0.8, 0.8) {
		t.Error("reported an unchanged score as a regression")
	}
	// THE FAILURE THIS PREVENTS. An inconclusive suite must not read as "every
	// test now fails", which would page an owner every time the model was
	// briefly unreachable.
	if regression(1.0, -1) {
		t.Error("an inconclusive rerun was reported as a regression")
	}
	if regression(-1, 0.0) {
		t.Error("a first measurement was reported as a regression")
	}
}

func TestStalenessIsAboutMeasurementsThatHappened(t *testing.T) {
	now := time.Now()
	measured := now.Add(-time.Hour)

	fresh := summary(3, 3, 3, &measured)
	markStale(fresh, now.Add(-2*time.Hour)) // edited before measuring
	if fresh.Stale {
		t.Error("a current measurement was marked out of date")
	}

	old := summary(3, 3, 3, &measured)
	markStale(old, now) // edited after measuring
	if !old.Stale {
		t.Error("a measurement that predates the last edit was not marked stale")
	}

	// Never measured is not the same as out of date, and calling it stale would
	// put "out of date" on a number that has never existed.
	never := summary(3, 0, 0, nil)
	markStale(never, now)
	if never.Stale {
		t.Error("an unmeasured agent was marked stale")
	}

	markStale(nil, now) // must not panic
}
