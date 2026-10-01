package codepr

// Quality metrics — turning recorded coding-run outcomes into a scorecard the
// admin can trust and the team can improve against. This is the measurement
// half of "outperform" (a static cloud agent can't learn from YOUR review
// signal): merge rate, verify-pass rate, scope discipline, draft rate, graded
// with a minimum-sample guard so a couple of runs never reads as confident.
//
// Pure + deterministic + zero-run safe (no NaN / divide-by-zero). Sourced from
// the code_pr_runs ledger (+ the PR's later merge state); the webhook capture of
// merge state and the learned-steering loop are separate slices (see
// .kiro/specs/agent-code-eval-loop).

// PROutcome is the terminal state of an agent PR (ground truth for quality).
type PROutcome string

const (
	OutcomeUnknown      PROutcome = ""                  // no PR yet / not resolved / no webhook
	OutcomeMerged       PROutcome = "merged"            // shipped as the agent wrote it
	OutcomeMergedEdited PROutcome = "merged_with_edits" // shipped after human fixes
	OutcomeClosed       PROutcome = "closed_unmerged"   // rejected
	OutcomeOpen         PROutcome = "open"              // still under review
)

// RunSignal is the outcome facts of one coding run, derived from a code_pr_runs
// row (+ PR merge state). Everything the scorecard needs, nothing it doesn't.
type RunSignal struct {
	Status    string // codepr Status* code
	HasPR     bool
	AllPassed bool
	HadTests  bool
	InScope   bool
	Draft     bool
	Outcome   PROutcome
}

// SignalFromRun maps a code_pr_runs-shaped record to a RunSignal. Kept here (not
// in the model layer) so the mapping is pure + testable and the model stays a
// thin persistence layer. hasPR is derived from a non-empty PR url.
func SignalFromRun(status, prURL string, allPassed, hadTests, inScope, draft bool, outcome PROutcome) RunSignal {
	return RunSignal{
		Status:    status,
		HasPR:     prURL != "",
		AllPassed: allPassed,
		HadTests:  hadTests,
		InScope:   inScope,
		Draft:     draft,
		Outcome:   outcome,
	}
}

// Scorecard is the aggregated quality view over a set of runs. Counts are exact;
// rates are in [0,1] and 0 when there is nothing to divide by.
type Scorecard struct {
	Total     int `json:"total"`
	Opened    int `json:"opened"`     // opened a PR
	Verified  int `json:"verified"`   // full verifier suite passed
	WithTests int `json:"with_tests"` // tests were present + ran
	InScope   int `json:"in_scope"`   // scope judge did not flag drift
	Draft     int `json:"draft"`      // opened as a draft (couldn't fully verify)
	NoGreen   int `json:"no_green"`   // couldn't reach a passing build/tests
	Blocked   int `json:"blocked"`    // needs_human / budget pause
	Failed    int `json:"failed"`     // error/timeout/too-large/unavailable

	// Ground-truth PR outcomes (require the webhook capture; unknowns excluded
	// from MergeRate's denominator).
	Merged          int `json:"merged"`
	MergedWithEdits int `json:"merged_with_edits"`
	Closed          int `json:"closed"`
	OutcomeKnown    int `json:"outcome_known"`

	OpenRate    float64 `json:"open_rate"`     // Opened / Total
	VerifyRate  float64 `json:"verify_rate"`   // Verified / Opened
	InScopeRate float64 `json:"in_scope_rate"` // InScope / Opened
	DraftRate   float64 `json:"draft_rate"`    // Draft / Opened
	MergeRate   float64 `json:"merge_rate"`    // (Merged + MergedWithEdits) / OutcomeKnown
}

// Aggregate computes a Scorecard from run signals. Pure, deterministic, and safe
// on empty input (all zeros, no NaN).
func Aggregate(signals []RunSignal) Scorecard {
	var s Scorecard
	s.Total = len(signals)
	for _, r := range signals {
		if r.HasPR {
			s.Opened++
			if r.AllPassed {
				s.Verified++
			}
			if r.HadTests {
				s.WithTests++
			}
			if r.InScope {
				s.InScope++
			}
			if r.Draft {
				s.Draft++
			}
		}
		switch r.Status {
		case StatusNoGreen:
			s.NoGreen++
		case StatusBlocked:
			s.Blocked++
		case StatusError, StatusTimeout, StatusTooLarge, StatusUnavailable:
			s.Failed++
		}
		switch r.Outcome {
		case OutcomeMerged:
			s.Merged++
			s.OutcomeKnown++
		case OutcomeMergedEdited:
			s.MergedWithEdits++
			s.OutcomeKnown++
		case OutcomeClosed:
			s.Closed++
			s.OutcomeKnown++
		}
	}

	s.OpenRate = safeRate(s.Opened, s.Total)
	s.VerifyRate = safeRate(s.Verified, s.Opened)
	s.InScopeRate = safeRate(s.InScope, s.Opened)
	s.DraftRate = safeRate(s.Draft, s.Opened)
	s.MergeRate = safeRate(s.Merged+s.MergedWithEdits, s.OutcomeKnown)
	return s
}

// safeRate returns num/den in [0,1], or 0 when den <= 0 (zero-run safe).
func safeRate(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Health grades for a scorecard.
const (
	GradeHealthy        = "healthy"
	GradeNeedsAttention = "needs_attention"
	GradeUnproven       = "unproven"
)

// Grade returns a coarse, honest health label for a scorecard. Below minSample
// runs it is always "unproven" (a couple of runs never reads as confident).
// Above the threshold it is "healthy" when the agent reliably opens verified,
// in-scope PRs (and, when merge outcomes are known, they mostly merge), else
// "needs_attention". Pure.
func Grade(s Scorecard, minSample int) string {
	if minSample < 1 {
		minSample = 1
	}
	if s.Total < minSample {
		return GradeUnproven
	}
	// Healthy bar: most runs produce a PR, nearly all PRs verify + stay in scope.
	healthy := s.OpenRate >= 0.7 && s.VerifyRate >= 0.9 && s.InScopeRate >= 0.9
	// When we have enough merge outcomes, require a decent merge rate too.
	if s.OutcomeKnown >= minSample {
		healthy = healthy && s.MergeRate >= 0.7
	}
	if healthy {
		return GradeHealthy
	}
	return GradeNeedsAttention
}
