package business

// Flip-centred regression detection.
//
// The watch compared pass RATES. An aggregate is the wrong instrument for this
// question, and not by a little: a suite that fixes one scenario and breaks
// another reports an identical rate while something the owner cared about has
// stopped working. The published guidance on gating self-improvement loops calls
// the alternative flip-centred gating, and the reason is exactly this. What
// matters is which cases changed verdict, not what the average did.
//
// It is also the difference between a message somebody can act on and one they
// cannot. "Passes 7/10, was 8/10" invites a shrug. "Refund wording no longer
// passes" is a bug report.

import (
	"github.com/google/uuid"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// EvalFlips is what changed between two runs of the same suite.
type EvalFlips struct {
	// Broke is scenarios that passed before and do not now. The signal.
	Broke []string
	// Fixed is scenarios that failed before and pass now. Carried so a report
	// can be honest about an edit that traded one for the other, which a bare
	// regression warning would present as pure loss.
	Fixed []string
}

// Regressed reports whether anything that used to work stopped working.
//
// Deliberately not "did the rate fall". An edit that breaks one case and fixes
// two has a better rate and still broke something, and the owner is entitled to
// know which.
func (f EvalFlips) Regressed() bool { return len(f.Broke) > 0 }

// computeEvalFlips diffs the verdicts before a suite ran against its results.
//
// Only conclusive-to-conclusive transitions count. A scenario that was never run
// is a first measurement rather than a regression, and one that went
// inconclusive in either direction is a scenario nobody scored, not a scenario
// that got worse: reporting either as a break would train the owner to ignore
// the warning, which costs more than the warning is worth.
func computeEvalFlips(before map[uuid.UUID]model.ScenarioVerdict, after []ScenarioRunResult) EvalFlips {
	var flips EvalFlips
	for _, a := range after {
		if a.Result.Inconclusive {
			continue
		}
		prev, ok := before[a.ScenarioId]
		if !ok || !prev.HasResult || prev.Inconclusive {
			continue
		}
		switch {
		case prev.Passed && !a.Result.Passed:
			flips.Broke = append(flips.Broke, a.Name)
		case !prev.Passed && a.Result.Passed:
			flips.Fixed = append(flips.Fixed, a.Name)
		}
	}
	return flips
}
