package codepr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// baseOrch builds an orchestrator with sensible fakes: resolves to one repo, no
// budget cap, judge in-scope, PR opener returns a URL, records into a sink.
func baseOrch(runner CodingRunner) (*Orchestrator, *[]Outcome) {
	var recorded []Outcome
	o := &Orchestrator{
		Runner: runner,
		Resolve: func(_ context.Context, _ string, _ Credential) RepoResolution {
			return RepoResolution{Found: true, Resolved: RepoRef{Owner: "o", Name: "r"}}
		},
		Budget: func(_ context.Context, _ Task) (BudgetInput, error) { return BudgetInput{}, nil },
		MintToken: func(_ context.Context, _ Task, _ RepoRef) (Credential, error) {
			return Credential{Token: "tok", Kind: CredentialUserConnector}, nil
		},
		Judge: func(_ context.Context, _, _ string) (Verdict, error) { return Verdict{InScope: true}, nil },
		OpenPR: func(_ context.Context, _ RepoRef, _, _, _, _ string, _ bool) (string, error) {
			return "https://github.com/o/r/pull/1", nil
		},
		Record: func(_ context.Context, _ Task, out Outcome, _ int) { recorded = append(recorded, out) },
		RunID:  "run123",
	}
	return o, &recorded
}

func task() Task {
	return Task{Instruction: "fix the padding on onecamp-fe", BaseBranch: "beta", AgentDisplayName: "Vella", RequestedByName: "Akash"}
}

func TestRun_HappyPathOpensPR(t *testing.T) {
	o, rec := baseOrch(&mockCodingRunner{Result: okResult()})
	out := o.Run(context.Background(), task())
	if out.Status != StatusOK || out.PRURL == "" {
		t.Fatalf("expected an opened PR, got %+v", out)
	}
	if !strings.Contains(out.Message, "Opened a pull request") || !strings.Contains(out.Message, "tests pass") {
		t.Fatalf("unexpected success message: %q", out.Message)
	}
	if len(*rec) != 1 {
		t.Fatalf("run should be recorded once, got %d", len(*rec))
	}
}

func TestRun_JobNeverPushesToBase(t *testing.T) {
	m := &mockCodingRunner{Result: okResult()}
	o, _ := baseOrch(m)
	o.Run(context.Background(), task())
	if m.LastJob.HeadBranch == m.LastJob.BaseBranch || m.LastJob.HeadBranch == "beta" {
		t.Fatalf("head branch must be fresh and never the base: %+v", m.LastJob)
	}
	if !strings.HasPrefix(m.LastJob.HeadBranch, "onecamp-agent/") || m.LastJob.CloneToken != "tok" {
		t.Fatalf("job not constructed as expected: %+v", m.LastJob)
	}
}

func TestRun_AmbiguousRepoBlocks(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.Resolve = func(_ context.Context, _ string, _ Credential) RepoResolution {
		return RepoResolution{Candidates: []RepoRef{{Owner: "o", Name: "a"}, {Owner: "o", Name: "b"}}}
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked || !strings.Contains(out.NeedsHumanQuestion, "Which repository") {
		t.Fatalf("ambiguous repo should block with a question, got %+v", out)
	}
}

func TestRun_NoneLinkedBlocks(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.Resolve = func(_ context.Context, _ string, _ Credential) RepoResolution {
		return RepoResolution{NoneLinked: true}
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked || !strings.Contains(out.Message, "No GitHub repository") {
		t.Fatalf("none-linked should block, got %+v", out)
	}
}

func TestRun_BudgetExhaustedBlocks(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.Budget = func(_ context.Context, _ Task) (BudgetInput, error) {
		return BudgetInput{WorkspaceCap: TierBudget{Runs: 1}, WorkspaceUsage: TierUsage{Runs: 1}}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked || out.StopReason != StopReasonCodingBudgetWorkspace {
		t.Fatalf("over-budget should block with typed reason, got %+v", out)
	}
}

func TestRun_TokenMintFailureBlocks(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.MintToken = func(_ context.Context, _ Task, _ RepoRef) (Credential, error) {
		return Credential{}, errors.New("no access")
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked || !strings.Contains(out.NeedsHumanQuestion, "GitHub") {
		t.Fatalf("a missing credential should block with an actionable message, got %+v", out)
	}
}

// TestRun_UnusableCredentialBlocksWithoutFallback pins the security decision: when
// no per-user credential is available the run BLOCKS. It must never silently fall
// back to the workspace admin's GitHub connection, which would attribute the
// commit to an admin who never wrote it and hand the run that admin's reach across
// every organisation they belong to.
func TestRun_UnusableCredentialBlocksWithoutFallback(t *testing.T) {
	runner := &mockCodingRunner{Result: okResult()}
	o, _ := baseOrch(runner)
	// No error — just nothing usable, which is what an unconnected user looks like.
	o.MintToken = func(_ context.Context, _ Task, _ RepoRef) (Credential, error) {
		return Credential{Kind: CredentialNone}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked {
		t.Fatalf("an unusable credential must block, got %+v", out)
	}
	if out.Credential != CredentialNone {
		t.Fatalf("a blocked run must not claim an identity, got %q", out.Credential)
	}
	if runner.Calls != 0 {
		t.Fatal("the runner must not execute without a usable credential — that is the fallback this removes")
	}
}

// TestRun_StampsTheCredentialOnTheOutcome proves every outcome carries which
// identity acted, so the audit row can answer it.
func TestRun_StampsTheCredentialOnTheOutcome(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	out := o.Run(context.Background(), task())
	if out.Credential != CredentialUserConnector {
		t.Fatalf("the outcome must record the identity the run acted as, got %q", out.Credential)
	}
}

func TestRun_OutOfScopePause(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.Policy = PolicyPause
	o.Judge = func(_ context.Context, _, _ string) (Verdict, error) {
		return Verdict{InScope: false, Concern: "also refactors unrelated files"}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked || !strings.Contains(out.NeedsHumanQuestion, "beyond the task") {
		t.Fatalf("pause policy should block on drift, got %+v", out)
	}
}

func TestRun_OutOfScopeFlagOpen(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.Policy = PolicyFlagOpen
	o.Judge = func(_ context.Context, _, _ string) (Verdict, error) {
		return Verdict{InScope: false, Concern: "touches an unrelated file"}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusOK || out.PRURL == "" {
		t.Fatalf("flag-open should still open a PR, got %+v", out)
	}
	if !strings.Contains(out.Message, "Heads up") {
		t.Fatalf("flag-open message should surface the concern: %q", out.Message)
	}
}

func TestRun_NoGreenNoDraft(t *testing.T) {
	res := okResult()
	res.Status = StatusNoGreen
	res.Verifier.AllPassed = false
	o, _ := baseOrch(&mockCodingRunner{Result: res})
	o.DraftOnRed = false
	out := o.Run(context.Background(), task())
	if out.Status != StatusNoGreen || out.PRURL != "" {
		t.Fatalf("no-green without draft policy must not open a PR, got %+v", out)
	}
}

func TestRun_NoGreenDraftOpens(t *testing.T) {
	res := okResult()
	res.Status = StatusNoGreen
	res.Verifier.AllPassed = false
	o, _ := baseOrch(&mockCodingRunner{Result: res})
	o.DraftOnRed = true
	out := o.Run(context.Background(), task())
	if out.Status != StatusOK || !out.Draft || out.PRURL == "" {
		t.Fatalf("no-green with draft policy should open a draft PR, got %+v", out)
	}
	if !strings.Contains(out.Message, "draft") {
		t.Fatalf("message should say draft: %q", out.Message)
	}
}

func TestRun_RunnerErrorIsHonest(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Err: errors.New("boom /work/deadbeefcafe1234/x")})
	out := o.Run(context.Background(), task())
	if out.Status != StatusError {
		t.Fatalf("runner error should map to error status, got %+v", out)
	}
	if strings.Contains(out.Message, "deadbeef") {
		t.Fatalf("host path must be sanitized from the message: %q", out.Message)
	}
}

func TestRun_PRAlreadyExists(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o.OpenPR = func(_ context.Context, _ RepoRef, _, _, _, _ string, _ bool) (string, error) {
		return "", ErrPullRequestExists
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusOK || !strings.Contains(out.Message, "already exists") {
		t.Fatalf("existing PR should be surfaced, got %+v", out)
	}
}

// timeoutResult is a run that exhausted its wall AFTER pushing the work it
// managed to finish — the runner reserves wrap-up time for exactly this, so a
// timeout now arrives with a branch, a diff, and honest verifier state.
func timeoutResult() CodingResult {
	res := okResult()
	res.Status = StatusTimeout
	res.Verifier.AllPassed = false
	res.Message = "I ran out of time before finishing, so I pushed the work I had completed for review."
	return res
}

func TestRun_TimeoutWithBranchHandsBackTheWork(t *testing.T) {
	o, rec := baseOrch(&mockCodingRunner{Result: timeoutResult()})
	o.DraftOnRed = false
	out := o.Run(context.Background(), task())
	if out.Status != StatusTimeout || out.PRURL != "" {
		t.Fatalf("timeout without the draft policy must not open a PR, got %+v", out)
	}
	if out.HeadBranch == "" || out.BranchURL == "" || out.PRCreateURL == "" {
		t.Fatalf("a pushed branch must come back as links, not a dead end: %+v", out)
	}
	if !strings.Contains(out.Message, out.PRCreateURL) || !strings.Contains(out.Message, "ran out of time") {
		t.Fatalf("message should be honest and carry the one-click link: %q", out.Message)
	}
	if out.DiffStat.Files == 0 || out.Verifier.AllPassed {
		t.Fatalf("partial work's diff/verifier state must survive: %+v", out)
	}
	if len(*rec) != 1 {
		t.Fatalf("a timeout that pushed work should be recorded once, got %d", len(*rec))
	}
}

func TestRun_TimeoutWithBranchOpensDraftWhenPolicyAllows(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: timeoutResult()})
	o.DraftOnRed = true
	out := o.Run(context.Background(), task())
	if out.Status != StatusOK || !out.Draft || out.PRURL == "" {
		t.Fatalf("timeout with a branch + draft policy should open a draft PR, got %+v", out)
	}
}

func TestRun_TimeoutWithoutBranchStaysHonest(t *testing.T) {
	res := timeoutResult()
	res.HeadBranch = ""
	res.DiffStat = DiffStat{}
	res.Message = ""
	o, _ := baseOrch(&mockCodingRunner{Result: res})
	o.DraftOnRed = true // irrelevant: there is nothing to open a PR from
	out := o.Run(context.Background(), task())
	if out.Status != StatusTimeout || out.PRURL != "" || out.BranchURL != "" || out.PRCreateURL != "" {
		t.Fatalf("a timeout with no branch must not invent links, got %+v", out)
	}
	if !strings.Contains(out.Message, "time limit") {
		t.Fatalf("message should say it ran out of time: %q", out.Message)
	}
}

func TestRun_TooLargeHonest(t *testing.T) {
	res := okResult()
	res.Status = StatusTooLarge
	res.Message = "repo too large"
	o, _ := baseOrch(&mockCodingRunner{Result: res})
	out := o.Run(context.Background(), task())
	if out.Status != StatusTooLarge || out.PRURL != "" {
		t.Fatalf("too-large should not open a PR, got %+v", out)
	}
}

func TestRun_Misconfigured(t *testing.T) {
	o := &Orchestrator{} // no runner/resolve
	out := o.Run(context.Background(), task())
	if out.Status != StatusError {
		t.Fatalf("misconfigured orchestrator should error cleanly, got %+v", out)
	}
}

// TestRun_ResolutionIsAuthorisedAsTheActingIdentity pins the ordering that makes
// the authorisation check mean something. The credential is established BEFORE the
// repository is resolved and is handed to resolution, so "may this run work on that
// repository" is answered for the identity that will actually push. Resolution used
// to run first and check with the workspace admin's credential, so an unlinked
// repository could be cleared because the ADMIN's account could see it and then be
// worked on by a member who could not.
func TestRun_ResolutionIsAuthorisedAsTheActingIdentity(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	var sawCredential Credential
	o.Resolve = func(_ context.Context, _ string, cred Credential) RepoResolution {
		sawCredential = cred
		return RepoResolution{Found: true, Resolved: RepoRef{Owner: "o", Name: "r"}}
	}
	if out := o.Run(context.Background(), task()); out.Status != StatusOK {
		t.Fatalf("expected the happy path, got %+v", out)
	}
	if !sawCredential.Usable() || sawCredential.Kind != CredentialUserConnector {
		t.Fatalf("resolution must receive the acting identity, got %+v", sawCredential)
	}
}

// A run with no usable identity must be refused BEFORE the repository is resolved:
// there is no point asking GitHub anything, and resolution would otherwise have to
// pick some other credential to ask with.
func TestRun_NoIdentityRefusesBeforeResolving(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	resolved := false
	o.Resolve = func(_ context.Context, _ string, _ Credential) RepoResolution {
		resolved = true
		return RepoResolution{Found: true, Resolved: RepoRef{Owner: "o", Name: "r"}}
	}
	o.MintToken = func(_ context.Context, _ Task, _ RepoRef) (Credential, error) {
		return Credential{Kind: CredentialNone, BlockedReason: "not connected"}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked {
		t.Fatalf("no identity must block, got %+v", out)
	}
	if resolved {
		t.Fatal("resolution must not run without an identity to authorise it as")
	}
}

// The second bind matters: provisional selection only proves the person has a
// GitHub connection, so a repository they cannot push to must still be refused
// after resolution — before any runner work is spent.
func TestRun_RepoBoundCheckRefusesAfterResolution(t *testing.T) {
	runner := &mockCodingRunner{Result: okResult()}
	o, _ := baseOrch(runner)
	o.MintToken = func(_ context.Context, _ Task, repo RepoRef) (Credential, error) {
		if !repo.Valid() {
			// Provisional: connected, repository not yet known.
			return Credential{Token: "tok", Kind: CredentialUserConnector}, nil
		}
		// Bound to the resolved repository: no write access there.
		return Credential{Kind: CredentialNone, BlockedReason: "you have read access but not write access"}, nil
	}
	out := o.Run(context.Background(), task())
	if out.Status != StatusBlocked {
		t.Fatalf("a repository the person cannot push to must block, got %+v", out)
	}
	if !strings.Contains(out.NeedsHumanQuestion, "write access") {
		t.Fatalf("the refusal must carry the specific reason, got %q", out.NeedsHumanQuestion)
	}
	if runner.Calls != 0 {
		t.Fatal("no runner work may be spent on a repository the run cannot push to")
	}
}
