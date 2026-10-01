package codesandbox

import "context"

// MockRunner is an in-memory Runner for tests: it records the last Job it
// received and returns a preset Result / error. It never executes code, so the
// entire server-side path (input resolution, budgets, output classification,
// audit) can be tested without Docker or any execution surface.
//
// Exported so tests in dependent packages (the orchestrator, the tool
// executor) can drive the whole flow deterministically.
type MockRunner struct {
	// Result is returned from Run when Err is nil.
	Result Result
	// Err, when set, is returned from Run (simulating a runner that is down or
	// unreachable). Result is ignored in that case.
	Err error
	// LastJob captures the most recent Job passed to Run, for assertions.
	LastJob Job
	// Calls counts how many times Run was invoked.
	Calls int
}

// Run records the job and returns the preset outcome.
func (m *MockRunner) Run(_ context.Context, job Job) (Result, error) {
	m.LastJob = job
	m.Calls++
	if m.Err != nil {
		return Result{}, m.Err
	}
	return m.Result, nil
}

// compile-time assertion that MockRunner satisfies Runner.
var _ Runner = (*MockRunner)(nil)
