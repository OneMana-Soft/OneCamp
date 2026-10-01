package codepr

// Learned steering — the compounding edge a static cloud agent can't have: turn
// THIS workspace's own review outcomes on a repo (merge rate, scope-drift rate,
// verify-fail rate, draft rate — from the code_pr_runs ledger) into a few
// concrete, human-readable rules, and inject them into the coding prompt for
// that repo's FUTURE runs. Over time the agent adapts to how a given team
// actually reviews, without any model retraining.
//
// This file is the PURE, deterministic distiller + formatter (fully unit-tested
// on a Scorecard). The per-repo signal read + the orchestrator injection seam
// are wired in the model/live layers; the distiller itself does no I/O.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// maxSteeringRules caps how many learned rules we inject, so the preamble
	// stays a focused nudge, not a wall of text that crowds out the task.
	maxSteeringRules = 4
	// minSteeringSample is the run count below which we never distill a rule —
	// a couple of runs is noise, not a pattern.
	minSteeringSample = 5
	// minOutcomeSample gates the merge-rate rule specifically (needs enough
	// resolved PRs, not just opened ones).
	minOutcomeSample = 3

	// Rate thresholds below/above which a corrective rule is warranted.
	steeringMergeFloor   = 0.5 // merged/known below this → "often rejected"
	steeringScopeFloor   = 0.8 // in-scope below this → "drifts"
	steeringVerifyFloor  = 0.8 // verified below this → "builds fail"
	steeringDraftCeiling = 0.3 // draft above this → "often unverifiable"
)

// steeringRule pairs the rendered rule text with a severity used only to order
// rules deterministically (highest-signal first) before capping.
type steeringRule struct {
	text     string
	severity int
}

// DistillSteering turns a repo's aggregated review Scorecard into a small,
// ordered set of corrective rules for the coding prompt. It is CORRECTIVE only:
// a healthy repo (or too little data) yields nil, so the agent is nudged only
// where this team's history shows a real, describable weakness. Pure +
// deterministic.
func DistillSteering(s Scorecard) []string {
	if s.Total < minSteeringSample {
		return nil
	}

	var rules []steeringRule

	// Ground truth first: PRs here get rejected → strongest nudge toward minimal,
	// convention-matching changes.
	if s.OutcomeKnown >= minOutcomeSample && s.MergeRate < steeringMergeFloor {
		rules = append(rules, steeringRule{
			text:     "Pull requests in this repository are often closed without merging — make the smallest change that fully solves the task, match the existing conventions exactly, and clearly justify the approach.",
			severity: 100,
		})
	}
	// Scope drift.
	if s.Opened >= minOutcomeSample && s.InScopeRate < steeringScopeFloor {
		rules = append(rules, steeringRule{
			text:     "Past changes here were flagged for going beyond the task — touch only what the task strictly requires; do not refactor, reformat, or bump dependencies that were not requested.",
			severity: 90,
		})
	}
	// Verification failures.
	if s.Opened >= minOutcomeSample && s.VerifyRate < steeringVerifyFloor {
		rules = append(rules, steeringRule{
			text:     "Changes here have frequently failed the repository's own build or tests — make sure format, lint, build, and tests all pass before finalizing, and never weaken or skip tests to get green.",
			severity: 80,
		})
	}
	// Unverifiable / draft-heavy.
	if s.Opened >= minOutcomeSample && s.DraftRate > steeringDraftCeiling {
		rules = append(rules, steeringRule{
			text:     "Changes here often could not be fully verified — prefer a smaller, self-contained change you can confirm builds and passes tests over a larger, unverifiable one.",
			severity: 70,
		})
	}

	if len(rules) == 0 {
		return nil
	}
	sort.SliceStable(rules, func(a, b int) bool { return rules[a].severity > rules[b].severity })
	out := make([]string, 0, maxSteeringRules)
	for _, r := range rules {
		out = append(out, r.text)
		if len(out) >= maxSteeringRules {
			break
		}
	}
	return out
}

// FormatSteeringBlock renders learned rules as a bounded, clearly-labeled
// preamble to prepend to a coding run's instruction, or "" when there are none
// (so the prompt is byte-identical to the no-history path). The block is framed
// as trusted guidance derived from the team's own review history — distinct from
// the UNTRUSTED task/repo content. Pure.
func FormatSteeringBlock(rules []string) string {
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Guidance learned from this repository's past review outcomes (follow it):\n")
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", r)
	}
	return strings.TrimRight(b.String(), "\n")
}
