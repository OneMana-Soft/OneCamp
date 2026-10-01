package business

import (
	"testing"

	"github.com/google/uuid"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func verdict(id uuid.UUID, name string, passed, inconclusive, hasResult bool) model.ScenarioVerdict {
	return model.ScenarioVerdict{ScenarioId: id, Name: name, Passed: passed, Inconclusive: inconclusive, HasResult: hasResult}
}

func scenarioResult(id uuid.UUID, name string, passed, inconclusive bool) ScenarioRunResult {
	return ScenarioRunResult{ScenarioId: id, Name: name, Result: ScoreResult{Passed: passed, Inconclusive: inconclusive}}
}

// The case the pass rate cannot see, and the reason this exists.
func TestFlipsCatchARegressionTheRateHides(t *testing.T) {
	broke, fixed := uuid.New(), uuid.New()
	before := map[uuid.UUID]model.ScenarioVerdict{
		broke: verdict(broke, "Refund wording", true, false, true),
		fixed: verdict(fixed, "Escalation path", false, false, true),
	}
	after := []ScenarioRunResult{
		scenarioResult(broke, "Refund wording", false, false),
		scenarioResult(fixed, "Escalation path", true, false),
	}

	flips := computeEvalFlips(before, after)

	// One broke, one was fixed: the pass rate is IDENTICAL before and after, and
	// a rate comparison reports nothing at all.
	if !flips.Regressed() {
		t.Fatal("a scenario that used to pass now fails and this was not reported")
	}
	if len(flips.Broke) != 1 || flips.Broke[0] != "Refund wording" {
		t.Fatalf("Broke = %v, want the one that stopped working by name", flips.Broke)
	}
	if len(flips.Fixed) != 1 || flips.Fixed[0] != "Escalation path" {
		t.Fatalf("Fixed = %v; an edit that trades one for another must not be reported as pure loss", flips.Fixed)
	}
}

func TestFlipsIgnoreNonSignals(t *testing.T) {
	fresh, wasInconclusive, nowInconclusive, steady := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	before := map[uuid.UUID]model.ScenarioVerdict{
		// Never run: a first failure is a first measurement, not a regression.
		fresh: verdict(fresh, "Brand new case", false, false, false),
		// Nobody scored it last time, so there is nothing to have got worse than.
		wasInconclusive: verdict(wasInconclusive, "Flaky case", false, true, true),
		nowInconclusive: verdict(nowInconclusive, "Passed before", true, false, true),
		steady:          verdict(steady, "Always passes", true, false, true),
	}
	after := []ScenarioRunResult{
		scenarioResult(fresh, "Brand new case", false, false),
		scenarioResult(wasInconclusive, "Flaky case", false, false),
		// Went inconclusive: unscored, not worse. Reporting it would train the
		// owner to ignore the warning, which costs more than the warning is worth.
		scenarioResult(nowInconclusive, "Passed before", false, true),
		scenarioResult(steady, "Always passes", true, false),
	}

	flips := computeEvalFlips(before, after)
	if flips.Regressed() {
		t.Fatalf("reported a regression for something that was not one: %v", flips.Broke)
	}
	if len(flips.Fixed) != 0 {
		t.Fatalf("Fixed = %v, want none", flips.Fixed)
	}
}

// With no prior verdicts there is nothing to diff, and the caller falls back to
// the rate. Silence here must not be mistaken for "nothing broke".
func TestFlipsWithNoHistoryAreEmpty(t *testing.T) {
	id := uuid.New()
	flips := computeEvalFlips(nil, []ScenarioRunResult{scenarioResult(id, "Anything", false, false)})
	if flips.Regressed() || len(flips.Fixed) != 0 {
		t.Fatalf("expected no flips without history, got broke=%v fixed=%v", flips.Broke, flips.Fixed)
	}
}
