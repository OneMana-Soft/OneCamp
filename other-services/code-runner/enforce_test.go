package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCappedWriter(t *testing.T) {
	w := newCappedWriter(10)
	n, _ := w.Write([]byte("hello")) // 5
	if n != 5 || w.Truncated() {
		t.Fatalf("unexpected after 5 bytes: n=%d trunc=%v", n, w.Truncated())
	}
	w.Write([]byte("world!!!")) // would be 13 > 10 → truncated at 10
	if !w.Truncated() {
		t.Fatal("should be truncated past the limit")
	}
	if got := w.String(); len(got) != 10 || !strings.HasPrefix(got, "helloworld") {
		t.Fatalf("capped content wrong: %q (len %d)", got, len(got))
	}
	// Further writes are discarded but pretend-accepted (so the child isn't
	// blocked on a full pipe).
	if n, _ := w.Write([]byte("more")); n != 4 {
		t.Fatalf("post-cap write should pretend-accept, got n=%d", n)
	}
	if len(w.String()) != 10 {
		t.Fatal("content grew past the cap")
	}
}

func TestBuildBootstrap(t *testing.T) {
	l := Limits{
		CPU:           int64(20 * time.Second),
		MemoryBytes:   512 << 20,
		PIDs:          64,
		ArtifactBytes: 2 << 20,
	}
	s := buildBootstrap(l, "/work/run-x/user.py")
	for _, want := range []string{
		"import resource, runpy",
		"resource.RLIMIT_CPU, 20",        // 20 CPU seconds
		"resource.RLIMIT_AS, 536870912",  // 512 MiB in bytes
		"resource.RLIMIT_NPROC, 64",      // pids
		"resource.RLIMIT_FSIZE, 2097152", // 2 MiB in bytes
		"resource.RLIMIT_NOFILE, 256",    // open files
		`runpy.run_path("/work/run-x/user.py", run_name='__main__')`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap missing %q:\n%s", want, s)
		}
	}
	// run_path must be last so limits apply before the user code runs.
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "runpy.run_path(") {
		t.Errorf("run_path must be the final line, got %q", lines[len(lines)-1])
	}
	if !strings.Contains(s, "except Exception:") {
		t.Error("setrlimit calls must be wrapped best-effort")
	}
}

func TestBuildBootstrap_FloorsTinyLimits(t *testing.T) {
	// Zero/negative limits must floor to safe minimums, never emit a 0 limit.
	s := buildBootstrap(Limits{}, "user.py")
	for _, bad := range []string{"RLIMIT_CPU, 0", "RLIMIT_NPROC, 0", "RLIMIT_AS, 0"} {
		if strings.Contains(s, bad) {
			t.Errorf("must floor tiny limits, found %q in:\n%s", bad, s)
		}
	}
}

func TestArtifactKindAndContentType(t *testing.T) {
	if artifactKind("chart.json") != KindChart {
		t.Error("json → chart candidate")
	}
	if artifactKind("data.csv") != KindFile {
		t.Error("csv → file")
	}
	if artifactKind("plot.png") != KindFile {
		t.Error("png → file")
	}
	if contentType("x.png") != "image/png" || contentType("x.csv") != "text/csv" ||
		contentType("x.json") != "application/json" || contentType("x.bin") != "application/octet-stream" {
		t.Error("content type mapping wrong")
	}
}

func TestStatusFor(t *testing.T) {
	cases := []struct {
		o    exitOutcome
		want string
	}{
		{exitOutcome{timedOut: true}, StatusTimeout},
		{exitOutcome{outputCapped: true}, StatusKilledLimit},
		{exitOutcome{timedOut: true, signaled: true}, StatusTimeout}, // timeout wins over signal
		{exitOutcome{signaled: true}, StatusOOM},
		{exitOutcome{exitCode: 0}, StatusOK},
		{exitOutcome{exitCode: 1}, StatusError},
	}
	for _, c := range cases {
		if got := statusFor(c.o); got != c.want {
			t.Errorf("statusFor(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

// --- Honest test disclosure -------------------------------------------------
//
// The feature's whole claim is "verified by the repo's own build and tests". A
// green exit code does not support that claim on its own: an EMPTY suite passes.
// `go test ./...` on a module with no test files exits zero, as do pytest with
// nothing collected and `npm test --if-present` with no test script. Reporting
// HadTests=true there would put a false statement in the PR body, which is worse
// than no claim at all for a reviewer deciding how much scrutiny to apply.

func TestDetectNoTests_RecognizesEmptySuites(t *testing.T) {
	empty := []string{
		"?   example.com/x [no test files]",
		"no tests ran",
		"collected 0 items",
		"There are no tests to run.",
		"npm ERR! Missing script: \"test\"\nno test specified",
		"NO TEST FILES",
	}
	for _, out := range empty {
		if !detectNoTests(out) {
			t.Errorf("output should be recognized as an empty suite: %q", out)
		}
	}
	real := []string{
		"ok  \texample.com/x\t0.012s",
		"5 passed in 0.42s",
		"Tests:       12 passed, 12 total",
	}
	for _, out := range real {
		if detectNoTests(out) {
			t.Errorf("output of a suite that really ran must not be treated as empty: %q", out)
		}
	}
}

// TestRunVerifiers_DoesNotClaimTestsThatNeverRan drives the real gate runner with
// a passing "test" gate whose output says nothing ran, and asserts the report
// stays honest: the gate passed, so the change is not blocked, but HadTests is
// false so the PR body discloses that no tests backed it.
func TestRunVerifiers_DoesNotClaimTestsThatNeverRan(t *testing.T) {
	root := t.TempDir()
	gates := []verifierCmd{
		{Name: "go test", Kind: verifierTest, Argv: []string{"echo", "?   example.com/x [no test files]"}},
	}
	report := runVerifiers(context.Background(), root, gates, 30*time.Second, 64<<10)

	if !report.AllPassed {
		t.Fatalf("an empty suite still exits zero, so the gate passes; got %+v", report)
	}
	if report.HadTests {
		t.Fatal("a suite with no test files must NOT be reported as tests that passed")
	}
	if len(report.Ran) != 1 || !strings.Contains(report.Ran[0].Summary, "no tests ran") {
		t.Fatalf("the summary must disclose that nothing ran; got %+v", report.Ran)
	}
}

// TestRunVerifiers_ClaimsTestsThatDidRun is the other half: a real suite must
// still set HadTests, or the disclosure would be uselessly pessimistic.
func TestRunVerifiers_ClaimsTestsThatDidRun(t *testing.T) {
	root := t.TempDir()
	gates := []verifierCmd{
		{Name: "go test", Kind: verifierTest, Argv: []string{"echo", "ok  \texample.com/x\t0.012s"}},
	}
	report := runVerifiers(context.Background(), root, gates, 30*time.Second, 64<<10)
	if !report.AllPassed || !report.HadTests {
		t.Fatalf("a suite that really ran and passed must be reported as such; got %+v", report)
	}
}
