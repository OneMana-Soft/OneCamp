package business

// Code-PR reliability scorecard — the admin-facing, HONEST measurement of how
// well the coding agent actually performs: how often it opens a PR, how often
// those PRs verify, stay in scope, are drafts, and (ground truth) how often they
// merge. This is the answer to "are we better than a cloud agent?" that never
// over-claims: the pure metrics core (codepr.Aggregate/Grade) grades on a
// minimum-sample guard, so a handful of runs reads as "unproven" rather than a
// misleading 100%.
//
// Reads the code_pr_runs ledger (permission: admin-only route) and folds it
// through the tested pure scorecard. Zero-run safe (all zeros, grade=unproven).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	codeprDomain "github.com/akashc777/OneCamp/domain/CodePR"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// codePRScorecardMinSample is the run count below which the scorecard is graded
// "unproven" — enough signal that a grade is meaningful, small enough to be
// reachable early. Also gates the merge-rate requirement inside Grade.
const codePRScorecardMinSample = 10

// CodePRScorecardView is the admin reliability payload: the aggregated scorecard
// (flattened via embedding, so its json tags surface directly), the honest
// health grade, and the window the numbers cover.
type CodePRScorecardView struct {
	codepr.Scorecard
	Grade      string `json:"grade"`       // healthy / needs_attention / unproven
	MinSample  int    `json:"min_sample"`  // sample size below which grade is unproven
	WindowDays int    `json:"window_days"` // 0 = all time
}

// CodePRScorecard builds the reliability scorecard over runs in the last
// windowDays days (0 => all time). Bounded, read-only, and safe on an empty
// ledger. windowDays < 0 is treated as 0.
func CodePRScorecard(ctx context.Context, windowDays int) (*CodePRScorecardView, error) {
	if windowDays < 0 {
		windowDays = 0
	}

	rows, err := codeprDomain.ListCodePRRunSignals(ctx, windowDays, 0)
	if err != nil {
		return nil, err
	}

	signals := make([]codepr.RunSignal, 0, len(rows))
	for _, r := range rows {
		signals = append(signals, codepr.SignalFromRun(
			r.Status, r.PRURL, r.AllPassed, r.HadTests, r.InScope, r.Draft, codepr.PROutcome(r.Outcome),
		))
	}

	sc := codepr.Aggregate(signals)
	return &CodePRScorecardView{
		Scorecard:  sc,
		Grade:      codepr.Grade(sc, codePRScorecardMinSample),
		MinSample:  codePRScorecardMinSample,
		WindowDays: windowDays,
	}, nil
}

// codePRRunnerProbeTimeout bounds the deployment self-test so a wedged runner
// endpoint can't hang the admin request.
const codePRRunnerProbeTimeout = 6 * time.Second

// TestCodePRRunner validates the configured code-PR runner deployment: it hits
// the runner's liveness endpoint to confirm reachability and reports whether the
// auth token is set. It does NOT trigger a real coding run (that clones/edits/
// pushes) — this is the safe "is my runner wired?" probe an admin runs right
// after deploying, mirroring TestSandbox for the python runner. Reads saved
// settings, so save the URL/token first.
func TestCodePRRunner(ctx context.Context) adapter.CodePRTestResult {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return adapter.CodePRTestResult{Ok: false, Status: "error", Message: "could not read AI settings"}
	}
	base := strings.TrimRight(strings.TrimSpace(settings.CodePRRunnerURL), "/")
	tokenSet := len(settings.CodePRRunnerTokenEnc) > 0
	if base == "" {
		return adapter.CodePRTestResult{
			Ok:      false,
			Status:  "unconfigured",
			Message: "Set and save a coding runner URL before testing.",
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, codePRRunnerProbeTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(probeCtx, http.MethodGet, base+"/healthz", nil)
	if rerr != nil {
		return adapter.CodePRTestResult{Ok: false, Status: "error", Message: "invalid runner URL", TokenSet: tokenSet}
	}

	start := time.Now()
	resp, derr := http.DefaultClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if derr != nil {
		return adapter.CodePRTestResult{
			Ok:        false,
			Status:    "unreachable",
			Message:   "Could not reach the coding runner at its configured URL. Check that the runner is deployed and the URL is correct.",
			LatencyMS: latency,
			TokenSet:  tokenSet,
		}
	}
	defer resp.Body.Close()

	endpointOK := resp.StatusCode >= 200 && resp.StatusCode < 500
	if !endpointOK {
		return adapter.CodePRTestResult{
			Ok:        false,
			Status:    "error",
			Message:   fmt.Sprintf("The runner endpoint responded with %d.", resp.StatusCode),
			LatencyMS: latency,
			TokenSet:  tokenSet,
		}
	}

	msg := "The coding runner is reachable."
	if !tokenSet {
		msg += " Warning: no runner auth token is set — set one so only OneCamp can dispatch coding jobs."
	}
	return adapter.CodePRTestResult{
		Ok:         true,
		Status:     "ok",
		Message:    msg,
		LatencyMS:  latency,
		TokenSet:   tokenSet,
		EndpointOK: true,
	}
}

// RecentCodePRRuns returns the newest coding runs for the admin runs view,
// mapped to a transparent display shape. Bounded + read-only.
func RecentCodePRRuns(ctx context.Context, limit int) ([]adapter.CodePRRunView, error) {
	rows, err := aiModels.ListRecentCodePRRuns(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.CodePRRunView, 0, len(rows))
	for _, r := range rows {
		repo := r.RepoOwner
		if r.RepoName != "" {
			if repo != "" {
				repo += "/" + r.RepoName
			} else {
				repo = r.RepoName
			}
		}
		out = append(out, adapter.CodePRRunView{
			ID:        r.ID.String(),
			Repo:      repo,
			Status:    r.Status,
			Outcome:   r.Outcome,
			PRURL:     r.PRURL,
			Draft:     r.Draft,
			AllPassed: r.AllPassed,
			DiffFiles: r.DiffFiles,
			Message:   r.Message,
			CreatedAt: r.CreatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}
