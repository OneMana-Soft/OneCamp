package business

// Reruns an agent's eval suite after the agent changes.
//
// WHY. The harness has always existed and has only ever run when somebody
// clicked it. So the pass rate in the agents list describes whatever version of
// the agent was current the last time a human remembered to press the button,
// and the question an owner actually has after editing an instruction, "did
// that make it worse", has been unanswerable. A green badge next to an agent
// rewritten a minute ago is worse than no badge: it is a confident answer to a
// question nobody asked.
//
// WHAT IT DOES NOT DO. It does not change the agent, and it never will. Scoring
// is one thing and acting on a score is another, and only the first belongs in
// a background loop. Everything here is read-and-measure; a lesson learned from
// a bad score is a separate, human-approved decision.
//
// COST IS THE DESIGN CONSTRAINT. Every scenario is a model call, so a naive
// "rerun everything hourly" would quietly spend a customer's token budget on
// agents nobody touched. This reruns an agent ONLY when its own updated_at has
// moved past its last measurement, which means work happens in proportion to
// edits rather than to time. An untouched workspace costs nothing.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

const (
	// Long enough that a burst of edits settles into one rerun rather than one
	// per keystroke-save, short enough that a badge is not wrong for a whole day.
	evalWatchDefaultInterval = 30 * time.Minute

	// Let the rest of the server finish booting; nothing here is urgent.
	evalWatchInitialDelay = 5 * time.Minute

	// Hard ceiling on model calls per pass. Suites are unbounded in principle
	// (fifty scenarios per agent is allowed), so without a cap one pass could
	// issue hundreds of calls. Agents beyond the cap are picked up next tick.
	evalWatchMaxScenariosPerPass = 40

	// A newly edited agent is left alone briefly, so an owner mid-edit is not
	// chased by reruns of a half-finished instruction.
	evalWatchSettlePeriod = 10 * time.Minute
)

// StartEvalWatch launches the rerun loop. Safe to call once at startup; it
// self-gates every tick and is inert when AI is unavailable or when the feature
// is switched off. ctx drives graceful shutdown.
func StartEvalWatch(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(evalWatchInitialDelay):
		}
		ticker := time.NewTicker(evalWatchInterval())
		defer ticker.Stop()

		runEvalWatchPass(ctx)
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Agent eval watcher shutting down")
				return
			case <-ticker.C:
				runEvalWatchPass(ctx)
			}
		}
	}()
}

// evalWatchInterval reads AGENT_EVAL_WATCH_INTERVAL_MIN.
func evalWatchInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("AGENT_EVAL_WATCH_INTERVAL_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return evalWatchDefaultInterval
}

// evalWatchEnabled defaults ON, and off is one variable away.
//
// On by default because the alternative is the status quo: a number on screen
// that nobody can tell is out of date. The cost is bounded by staleness, so on
// a workspace where no agent is edited this does nothing at all.
func evalWatchEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_EVAL_WATCH_ENABLED")))
	return v != "false" && v != "0" && v != "no"
}

// runEvalWatchPass reruns the suite for every agent whose measurement predates
// its last edit, up to the per-pass scenario budget.
func runEvalWatchPass(ctx context.Context) {
	if !evalWatchEnabled() || !helpers.FeatureStatus()[helpers.FeatureNameAI] {
		return
	}

	// Admin actor: this is a system pass over every agent in the workspace, not
	// a user request. The RUN itself is still executed as the agent's own owner
	// with per-call permission re-checks, so nothing here widens what an agent
	// can reach; see runAndScore.
	sysActor := Actor{IsAdmin: true}

	agents, err := ListAgents(ctx, sysActor)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/evalWatch: list agents: %v", err)
		return
	}

	summaries, err := EvalSummaryBatch(ctx, sysActor)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/evalWatch: summaries: %v", err)
		return
	}

	budget := evalWatchMaxScenariosPerPass
	reran, regressed := 0, 0

	for _, a := range agents {
		if budget <= 0 {
			break
		}
		sum := summaries[a.Id.String()]
		if !shouldRerun(a, sum, time.Now()) {
			continue
		}

		before := passRate(sum)

		// As the agent's OWNER, so scenario runs resolve the same permissions the
		// agent has in production rather than an admin's wider set.
		ownerActor := Actor{UserID: a.CreatedBy}

		// Captured BEFORE the suite runs, because running it overwrites the very
		// verdicts being compared against. Failing to read them is not fatal: the
		// rate comparison below still works, it is just less specific.
		verdictsBefore, vErr := model.LatestScenarioVerdicts(ctx, a.Id)
		if vErr != nil {
			helpers.LogErrorWithContext(ctx, "business/evalWatch: could not read prior verdicts for %s: %v", a.Id, vErr)
			verdictsBefore = nil
		}

		res, runErr := RunSuite(ctx, a.Id, ownerActor)
		if runErr != nil {
			helpers.LogErrorWithContext(ctx, "business/evalWatch: agent %s suite: %v", a.Id, runErr)
			continue
		}

		budget -= res.Total
		reran++

		after := -1.0
		if res.Scored > 0 {
			after = float64(res.Passed) / float64(res.Scored)
		}
		// Flips first, rate second.
		//
		// An aggregate is the wrong instrument: a suite that fixes one case and
		// breaks another reports an identical rate while something the owner
		// cared about stopped working. The rate is kept as a fallback for the
		// first comparison after an upgrade, when there are no prior verdicts to
		// diff against and it is the only signal available.
		flips := computeEvalFlips(verdictsBefore, res.Scenarios)
		if flips.Regressed() || (len(verdictsBefore) == 0 && regression(before, after)) {
			regressed++
			// WARN, not ERROR: the agent is doing what it was configured to do and
			// the configuration is now worse. That is a message for its owner, not
			// a fault in this service.
			if flips.Regressed() {
				helpers.LogWarnWithContext(ctx,
					"agent eval regression: %q broke %d scenario(s) after an edit (%v), fixed %d, now %d/%d",
					a.Name, len(flips.Broke), flips.Broke, len(flips.Fixed), res.Passed, res.Scored)
			} else {
				helpers.LogWarnWithContext(ctx,
					"agent eval regression: %q now passes %d/%d after an edit (was %.0f%%)",
					a.Name, res.Passed, res.Scored, before*100)
			}

			// And tell the one person who can act on it. A warning in a server
			// log reaches whoever reads server logs, which is not the agent's
			// owner and on a self-hosted install is often nobody.
			notifyRegression(ctx, a, before, res.Passed, res.Scored)
		}
	}

	if reran > 0 {
		helpers.LogInfoWithContext(ctx,
			"agent eval watcher: reran %d suite(s), %d regressed", reran, regressed)
	}
}

// shouldRerun decides whether an agent's suite is worth spending model calls on.
//
// Split out and pure so the policy is testable without a database, which matters
// because every branch here is a decision about somebody's token budget.
func shouldRerun(a *model.AiAgent, sum *model.AgentEvalSummary, now time.Time) bool {
	if a == nil || sum == nil {
		return false // no agent, or no active scenarios: nothing to measure
	}
	if sum.ScenarioCount == 0 {
		return false
	}
	if sum.LastEvaluatedAt == nil {
		// Never measured. Leave the first run to a human: an owner who has written
		// scenarios and not run them may still be drafting, and spending their
		// budget uninvited is a bad first impression of the feature.
		return false
	}
	if !a.UpdatedAt.After(*sum.LastEvaluatedAt) {
		return false // measurement is current
	}
	// Edited very recently: let them finish.
	return now.Sub(a.UpdatedAt) >= evalWatchSettlePeriod
}

// passRate is the scored pass fraction, or -1 when nothing was scored.
//
// -1 rather than 0 because "no conclusive result" and "everything failed" are
// different facts, and treating the first as the second would report a
// regression every time a suite went inconclusive.
func passRate(sum *model.AgentEvalSummary) float64 {
	if sum == nil || sum.Scored == 0 {
		return -1
	}
	return float64(sum.Passed) / float64(sum.Scored)
}

// regression reports a real drop between two pass rates, ignoring the
// unmeasured sentinel on either side.
func regression(before, after float64) bool {
	if before < 0 || after < 0 {
		return false
	}
	return after < before
}
