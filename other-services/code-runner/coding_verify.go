package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// verifierWaitDelay bounds how long a finished gate may hold the run while its
// output pipes are still open by a process it left behind. Short enough that a
// straggler cannot stall the job, long enough that ordinary buffered output from
// a normally-exiting command is never truncated.
const verifierWaitDelay = 5 * time.Second

// coding_verify.go — detecting a repo's quality gates from its manifest files
// and running them. Detection is PURE + unit-tested; running is a thin,
// deadline-bounded exec whose output is summarized to the essential lines (the
// model never parses raw build logs). A non-draft PR requires these to pass.

// verifierCmd is one gate to run: a name, kind (fmt/lint/build/test), and argv.
type verifierCmd struct {
	Name string
	Kind string
	Argv []string
}

// detectVerifiers maps the presence of manifest files to a bounded, ordered set
// of gate commands (fmt → build → test). Generic across the common ecosystems;
// unknown repos yield nil (the run reports "no verifiers detected" honestly).
// Pure: it only inspects the provided file-name set.
func detectVerifiers(files map[string]bool) []verifierCmd {
	has := func(n string) bool { return files[n] }
	var out []verifierCmd

	switch {
	case has("go.mod"):
		out = append(out,
			verifierCmd{"go vet", verifierBuild, []string{"go", "vet", "./..."}},
			verifierCmd{"go build", verifierBuild, []string{"go", "build", "./..."}},
			verifierCmd{"go test", verifierTest, []string{"go", "test", "./..."}},
		)
	case has("package.json"):
		// Prefer the repo's own scripts; run build + test when present. The
		// caller only runs these if the script exists (npm exits non-zero for a
		// missing script, which we treat as "gate absent", not failure).
		out = append(out,
			verifierCmd{"npm run build", verifierBuild, []string{"npm", "run", "--if-present", "build"}},
			verifierCmd{"npm test", verifierTest, []string{"npm", "test", "--if-present"}},
		)
	case has("Cargo.toml"):
		out = append(out,
			verifierCmd{"cargo build", verifierBuild, []string{"cargo", "build"}},
			verifierCmd{"cargo test", verifierTest, []string{"cargo", "test"}},
		)
	case has("pyproject.toml"), has("setup.py"), has("requirements.txt"):
		out = append(out,
			verifierCmd{"python compile", verifierBuild, []string{"python3", "-m", "compileall", "-q", "."}},
			verifierCmd{"pytest", verifierTest, []string{"pytest", "-q"}},
		)
	case has("pom.xml"):
		out = append(out, verifierCmd{"maven verify", verifierBuild, []string{"mvn", "-q", "-B", "verify"}})
	case has("build.gradle"), has("build.gradle.kts"):
		out = append(out, verifierCmd{"gradle build", verifierBuild, []string{"gradle", "build", "--console=plain"}})
	}
	return out
}

// isTestVerifier reports whether a gate is a test gate (for HadTests reporting).
func isTestVerifier(v verifierCmd) bool { return v.Kind == verifierTest }

// scanRepoManifests returns the set of manifest file names present at the repo
// root, for detectVerifiers. Best-effort (read errors ⇒ empty set).
func scanRepoManifests(root string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			out[e.Name()] = true
		}
	}
	return out
}

// summarizeOutput reduces noisy build/test output to the essential lines: the
// last maxLines non-blank lines, preferring lines that look like errors. Pure.
func summarizeOutput(output string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 20
	}
	lines := strings.Split(output, "\n")
	// Collect error-ish lines first (bounded), then fall back to the tail.
	var errLines []string
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "error") || strings.Contains(ll, "fail") || strings.Contains(ll, "panic") || strings.Contains(ll, "cannot") {
			if t := strings.TrimSpace(l); t != "" {
				errLines = append(errLines, t)
			}
		}
	}
	pick := errLines
	if len(pick) == 0 {
		for _, l := range lines {
			if t := strings.TrimSpace(l); t != "" {
				pick = append(pick, t)
			}
		}
	}
	if len(pick) > maxLines {
		pick = pick[len(pick)-maxLines:]
	}
	return strings.TrimSpace(strings.Join(pick, "\n"))
}

// runVerifiers runs the detected gates in order in the checkout, each under a
// bounded deadline + secret-free env, and returns a report. A gate's failure
// short-circuits the rest (no point testing an unbuildable tree). HadTests is
// set when a test gate actually ran and passed. Output is summarized per gate.
func runVerifiers(ctx context.Context, root string, gates []verifierCmd, perGate time.Duration, outputCap int) CodingVerifierReport {
	if perGate <= 0 {
		perGate = 3 * time.Minute
	}
	report := CodingVerifierReport{AllPassed: true}
	if len(gates) == 0 {
		// No detected gates: honest — nothing verified. AllPassed stays true so
		// the orchestrator can still open a (clearly no-tests) PR, disclosed via
		// HadTests=false and an empty Ran list.
		report.AllPassed = true
		return report
	}
	for _, g := range gates {
		out, err := runOneVerifier(ctx, root, g, perGate, outputCap)
		passed := err == nil
		res := CodingVerifierResult{Name: g.Name, Kind: g.Kind, Passed: passed}
		if passed {
			res.Summary = "passed"
			// A test gate that exits zero has NOT necessarily run anything: an empty
			// suite is a pass. Only claim tests when the output doesn't say otherwise,
			// so "verified by the repo's own tests" stays a true statement.
			if isTestVerifier(g) && !detectNoTests(out) {
				report.HadTests = true
			}
			if isTestVerifier(g) && detectNoTests(out) {
				res.Summary = "passed (no tests ran)"
			}
		} else {
			res.Summary = summarizeOutput(out, 20)
			if strings.TrimSpace(res.Summary) == "" {
				res.Summary = "failed"
			}
			report.AllPassed = false
		}
		report.Ran = append(report.Ran, res)
		if !passed {
			break // short-circuit: don't test an unbuildable tree
		}
	}
	return report
}

// verifierTmpDir returns the scratch directory for a gate: a sibling of the
// checkout, so toolchain residue can never be picked up by `git add -A` and
// committed into the PR. Both the checkout and this sibling live under the
// per-run temp dir, so both are wiped together. If root has no usable parent
// (root is a filesystem root, which only happens in a degenerate test), it falls
// back to a dot-dir inside root — correctness of the run beats tidiness there.
// Pure: string logic only.
func verifierTmpDir(root string) string {
	clean := filepath.Clean(root)
	parent := filepath.Dir(clean)
	if parent == clean || parent == "." || parent == string(filepath.Separator) {
		return filepath.Join(clean, ".tmp")
	}
	return filepath.Join(parent, "verifier-tmp")
}

// verifierEnv builds the environment for a build/test gate: secret-free (so a
// test the model just wrote cannot read the runner's tokens), pointed at a
// scratch TMPDIR outside the work tree, PLUS the egress route.
//
// The route matters here for the same reason it matters to git: a dependency
// fetch — `go build` resolving modules, `npm` resolving packages — has no way out
// of the runner's network except the allowlisting proxy, so a gate without these
// variables fails on any repo with external dependencies even when the operator
// has allowlisted the registry. Pure, so the invariant is unit-tested.
func verifierEnv(root, tmpDir string) []string {
	return append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + root,
		"TMPDIR=" + tmpDir,
		"CI=1",
		"GOFLAGS=-mod=mod",
	}, proxyEnv()...)
}

// noTestsRe matches the common "nothing actually ran" signals a passing test gate
// emits when a repo has no tests. `go test ./...` on a module with no test files
// EXITS ZERO, as do pytest with no collected items and `npm test --if-present`
// with no test script — so exit status alone cannot tell "the suite is green"
// from "there is no suite", and reporting the former would be a lie in the PR
// body. Mirrors the server's DetectNoTests.
var noTestsRe = regexp.MustCompile(`(?i)(no test files|no tests ran|no tests found|collected 0 items|0 tests|there are no tests|no test specified)`)

// detectNoTests reports whether a passing test gate's output shows that no tests
// actually executed, so HadTests stays false and the PR discloses it. Pure.
func detectNoTests(output string) bool { return noTestsRe.MatchString(output) }

// runOneVerifier executes a single gate. Returns combined output + error.
func runOneVerifier(ctx context.Context, root string, g verifierCmd, timeout time.Duration, outputCap int) (string, error) {
	if len(g.Argv) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A per-run scratch TMPDIR that sits BESIDE the checkout, not inside it. It is
	// still wiped with the run (the parent temp dir is removed wholesale), but
	// keeping it out of the work tree matters: finalization stages with `git add
	// -A`, so a toolchain that leaves residue behind — routine when a gate is
	// killed at its deadline — would otherwise commit its scratch files into the
	// pull request. verifierTmpDir falls back to a path inside the checkout only
	// if the parent is somehow unusable.
	tmpDir := verifierTmpDir(root)
	_ = os.MkdirAll(tmpDir, 0o755)
	cmd := exec.CommandContext(cctx, g.Argv[0], g.Argv[1:]...)
	cmd.Dir = root
	cmd.Env = verifierEnv(root, tmpDir)
	// New process group, so hitting the gate's deadline takes down the whole tree
	// rather than just the command we launched. A build or test script routinely
	// spawns children (a compiler, a test server, a watcher), and
	// exec.CommandContext kills ONLY the direct child — so without this an
	// orphaned grandchild outlives its gate and keeps running for the rest of the
	// job. That costs two ways: it burns the container's pids/CPU/memory budget
	// so a LATER gate fails for an unrelated reason, and it leaves an attacker-
	// controlled process alive across the push, where a git child briefly carries
	// the repository credential in its argv (readable via /proc). The python
	// sandbox in exec.go has always done this; the coding profile — which runs the
	// repo's own scripts, so strictly less trusted — did not.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole GROUP when the deadline fires. exec.CommandContext's default
	// cancel signals only the process it launched, which leaves the tree running.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// And bound how long Wait will sit on the output pipes afterwards. Without
	// this a gate that backgrounds anything HANGS far past its own deadline: the
	// direct child exits immediately, but Wait keeps reading pipes the surviving
	// grandchild still holds open, so the per-gate timeout bounds nothing at all
	// and the run stalls until the straggler happens to finish.
	cmd.WaitDelay = verifierWaitDelay

	w := newCappedWriter(outputCap)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()

	// Best-effort sweep of anything the gate left behind, whether it timed out or
	// simply exited without waiting for its own children.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A straggler holding the pipes is not a gate FAILURE — the command itself
	// exited cleanly. Reporting it as failed would punish a suite that
	// legitimately backgrounds a helper process, and would send the model off
	// trying to "fix" a build that is fine.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		err = nil
	}
	return w.String(), err
}
