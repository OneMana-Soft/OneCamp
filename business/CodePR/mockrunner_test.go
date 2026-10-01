package codepr

import "context"

// mockCodingRunner is an in-memory CodingRunner for tests: it performs no git,
// no exec, and no network. It returns a fixed, caller-supplied result (or error)
// so the entire main-server decision layer — budgets, judge, PR authoring,
// outcome handling — can be proven before any real runner exists. The captured
// LastJob lets a test assert what the orchestrator handed to the runner (e.g.
// that the head branch is fresh and never the base branch, that the token is
// set, that limits/egress were applied).
type mockCodingRunner struct {
	Result  CodingResult
	Err     error
	LastJob CodingJob
	Calls   int
}

// Run records the job and returns the preset result/error.
func (m *mockCodingRunner) Run(_ context.Context, job CodingJob) (CodingResult, error) {
	m.LastJob = job
	m.Calls++
	if m.Err != nil {
		return CodingResult{}, m.Err
	}
	return m.Result, nil
}

// okResult builds a successful, verified result for tests (a small passing diff
// with tests present) — the happy path a PR is opened from.
func okResult() CodingResult {
	return CodingResult{
		Status:     StatusOK,
		HeadBranch: "onecamp-agent/fix-padding",
		DiffRef:    "diff:abc123",
		Diff:       "diff --git a/x b/x\n@@ -1 +1 @@\n-old\n+new\n",
		DiffStat:   DiffStat{Files: 1, Added: 3, Removed: 3},
		Verifier: VerifierReport{
			AllPassed: true,
			HadTests:  true,
			Ran: []VerifierResult{
				{Name: "gofmt", Kind: VerifierFormat, Passed: true, Summary: "ok"},
				{Name: "go build", Kind: VerifierBuild, Passed: true, Summary: "ok"},
				{Name: "go test", Kind: VerifierTest, Passed: true, Summary: "ok (12 tests)"},
			},
		},
		Usage: CodingUsage{WallMS: 42000, VerifyIterations: 2},
	}
}
