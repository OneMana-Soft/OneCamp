//go:build coderun_integration

// Package integration tests for the coding profile. GATED behind the
// `coderun_integration` build tag so the default `go test ./...` stays hermetic
// (no git / no toolchain dependency). Run in the deployed sandbox — where git,
// the language toolchains, and the isolation profile exist — with:
//
//	go test -tags coderun_integration ./...
//
// These exercise the REAL handler paths (git flow + the edit/verify loop) using
// a LOCAL file:// origin and a fake in-process LLM proxy, so they need only git
// (and, for the loop test, `go`) — no network, no real git host, no live model.
// The container-level isolation (egress deny, non-root, seccomp, gVisor,
// rlimits) is asserted at deploy time by the compose profile + a smoke run, not
// here; these prove the handler's OWN safety invariants against real git.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireGit skips the test when git is unavailable.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping integration test")
	}
}

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, 30*time.Second,
		append([]string{"-c", "init.defaultBranch=main", "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// setupOrigin creates a bare origin with one commit on main and returns its
// file:// URL. Used as the clone/push target so no network/host is needed.
func setupOrigin(t *testing.T) (originURL, workRoot string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	git(t, "", "init", "--bare", bare)
	git(t, "", "clone", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "go.mod"), []byte("module example.com/x\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-m", "seed")
	git(t, seed, "push", "origin", "HEAD:refs/heads/main")
	return "file://" + bare, root
}

// TestIntegration_GitFlowInvariants proves the clone → fresh branch → commit →
// push flow works against a real repo AND that the safety invariants hold: the
// base branch is never modified, the pushed ref is the head branch only, and no
// credential is persisted into .git/config.
func TestIntegration_GitFlowInvariants(t *testing.T) {
	requireGit(t)
	originURL, root := setupOrigin(t)

	repoDir := filepath.Join(root, "checkout")
	// Clone with a per-command auth header (dummy token; file:// ignores it) to
	// prove the token never lands in .git/config.
	args := append(authHeaderConfigArgs("ghp_dummy_secret_token"), cloneArgs(originURL, "main", repoDir, 1, false)...)
	if out, err := runGit(context.Background(), "", 2*time.Minute, args...); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	// Credential must NOT be persisted anywhere in the checkout's git config.
	cfg, _ := os.ReadFile(filepath.Join(repoDir, ".git", "config"))
	if strings.Contains(string(cfg), "ghp_dummy_secret_token") || strings.Contains(string(cfg), "extraheader") {
		t.Fatalf(".git/config must not persist the credential:\n%s", cfg)
	}

	// Fresh head branch (refuses base — assert the guard).
	if _, err := newBranchArgs("main", "main"); err == nil {
		t.Fatal("newBranchArgs must refuse head==base")
	}
	nb, err := newBranchArgs("onecamp-agent/feat-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, nb...)

	// Edit + commit via the real builders.
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nfunc main() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, _ := commitArgs("integration change")
	for _, c := range cmds {
		git(t, repoDir, c...)
	}

	// Push the head branch only, never force.
	push, err := pushArgs("onecamp-agent/feat-1")
	if err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, append(authHeaderConfigArgs("ghp_dummy_secret_token"), push...)...)

	// Origin must now have the head branch, and main must be unchanged (the run
	// can never move the base branch).
	bare := strings.TrimPrefix(originURL, "file://")
	branches := git(t, bare, "branch", "--list")
	if !strings.Contains(branches, "onecamp-agent/feat-1") {
		t.Fatalf("head branch not pushed to origin: %q", branches)
	}
	mainLog := git(t, bare, "log", "--oneline", "main")
	if strings.Contains(mainLog, "integration change") {
		t.Fatal("base branch main must NOT contain the agent commit")
	}
}

// TestIntegration_EditVerifyLoop drives the real edit/verify loop with a fake
// in-process LLM proxy that returns an edit fixing a deliberately-broken build,
// against a real go module — proving the loop applies edits and gates on the
// repo's own `go build`/`go vet`/`go test`.
func TestIntegration_EditVerifyLoop(t *testing.T) {
	requireGit(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A repo that does NOT build (undefined symbol), so the loop must fix it.
	writeFile(t, repoDir, "go.mod", "module example.com/x\n\ngo 1.23\n")
	writeFile(t, repoDir, "main.go", "package main\n\nfunc main() { println(missing) }\n")
	git(t, repoDir, "init")
	git(t, repoDir, "add", "-A")
	git(t, repoDir, "commit", "-m", "seed")

	// Fake LLM proxy: returns a full-file edit that makes the program compile.
	fixed := "package main\n\nfunc main() { println(\"ok\") }\n"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edit := map[string]any{
			"edits": []map[string]any{{"path": "main.go", "contents": fixed}},
			"done":  true,
		}
		body, _ := json.Marshal(edit)
		resp := map[string]string{"content": string(body)}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer proxy.Close()

	job := CodingJob{
		ID:            "it-1",
		Prompt:        "fix the build",
		Limits:        CodingLimits{MaxVerifyIters: 3, OutputBytes: 256 << 10},
		LLMProxyURL:   proxy.URL,
		LLMProxyToken: "x",
	}
	llm := newLLMClient(job.LLMProxyURL, job.LLMProxyToken, job.ID)
	// A soft deadline far enough out that the loop runs normally (the timeout
	// paths are covered by the hermetic tests + the wrap-up test below).
	softDeadline := time.Now().Add(10 * time.Minute)
	loop := editVerifyLoop(context.Background(), job, llm, repoDir, softDeadline)
	report, iters, ranOutOfTime := loop.Report, loop.Iterations, loop.RanOutOfTime
	if iters < 1 {
		t.Fatal("loop should run at least one iteration")
	}
	if ranOutOfTime {
		t.Fatal("a loop that reached green well inside its deadline must not report a timeout")
	}
	if !report.AllPassed {
		t.Fatalf("loop should reach green after the fix; report=%+v", report)
	}
	// The edit must have been applied verbatim.
	got, _ := os.ReadFile(filepath.Join(repoDir, "main.go"))
	if string(got) != fixed {
		t.Fatalf("edit not applied; got:\n%s", got)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIntegration_WrapUpSurvivesExpiredRunContext proves the split-deadline fix at
// the exact seam that used to lose work: once the RUN context is spent, the
// wrap-up context (funded by the reserve, derived from nothing) can still read the
// diff, commit, and push the branch. The old code derived finalization from the
// expired run context, so the diff read back empty, the run reported "I couldn't
// produce a change", and the checkout — with the real edits in it — was deleted.
func TestIntegration_WrapUpSurvivesExpiredRunContext(t *testing.T) {
	requireGit(t)
	originURL, root := setupOrigin(t)
	repoDir := filepath.Join(root, "checkout")
	if out, err := runGit(context.Background(), "", 2*time.Minute, cloneArgs(originURL, "main", repoDir, 1, false)...); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	nb, err := newBranchArgs("onecamp-agent/wrapup-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, nb...)
	// The model's work: an edit that exists only in the working tree at the moment
	// the run context dies.
	writeFile(t, repoDir, "main.go", "package main\n\nfunc main() { println(\"edited\") }\n")

	// The expired run context — exactly what the loop hands back at the deadline.
	runCtx, cancelRun := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelRun()
	if stat := computeDiffStat(runCtx, repoDir, 30*time.Second); stat.Files != 0 {
		t.Fatalf("an expired context cannot read a diff (that was the bug); got %+v", stat)
	}

	// The wrap-up context: independent, funded by the reserve.
	wrap := newWrapUpBudget(wrapUpReserve(15 * time.Minute))
	wrapCtx, cancelWrap := context.WithTimeout(context.Background(), wrap.Total)
	defer cancelWrap()
	stat := computeDiffStat(wrapCtx, repoDir, wrap.Diff)
	if stat.Files == 0 {
		t.Fatal("wrap-up must see the work the loop produced")
	}
	job := CodingJob{HeadBranch: "onecamp-agent/wrapup-1", Prompt: "wrap up partial work"}
	if res, ok := commitAndPush(wrapCtx, job, repoDir, wrap); !ok {
		t.Fatalf("wrap-up must land partial work on a branch: %+v", res)
	}
	bare := strings.TrimPrefix(originURL, "file://")
	if branches := git(t, bare, "branch", "--list"); !strings.Contains(branches, "onecamp-agent/wrapup-1") {
		t.Fatalf("partial work must be pushed to a branch: %q", branches)
	}
	if mainLog := git(t, bare, "log", "--oneline", "main"); strings.Contains(mainLog, "wrap up partial work") {
		t.Fatal("wrap-up must never commit onto the base branch")
	}
}

// TestIntegration_NewFilesOnlyChangeIsNotDiscarded is the regression test for a
// bug that threw away entire successful runs. `git diff` compares the WORK TREE
// to HEAD, and an untracked file appears in neither — so when the model's whole
// change was NEW files (adding an endpoint, a test, a migration: some of the most
// common coding tasks there are), the change measured as zero files. The run then
// reported "I couldn't produce a change for this task", pushed nothing, and
// deleted the checkout with the correct work still in it.
//
// The existing wrap-up test missed this because its fixture MODIFIES a tracked
// file, which `git diff` does see.
func TestIntegration_NewFilesOnlyChangeIsNotDiscarded(t *testing.T) {
	requireGit(t)
	originURL, root := setupOrigin(t)
	repoDir := filepath.Join(root, "checkout")
	if out, err := runGit(context.Background(), "", 2*time.Minute, cloneArgs(originURL, "main", repoDir, 1, false)...); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	nb, err := newBranchArgs("onecamp-agent/newfile-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, nb...)

	// The model's work: only brand-new files, no edits to anything tracked.
	writeFile(t, repoDir, "added.go", "package main\n\nfunc Added() {}\n")

	wrap := newWrapUpBudget(wrapUpReserve(15 * time.Minute))
	wrapCtx, cancel := context.WithTimeout(context.Background(), wrap.Total)
	defer cancel()

	stat := computeDiffStat(wrapCtx, repoDir, wrap.Diff)
	if stat.Files == 0 {
		t.Fatal("a change made entirely of new files must be counted, or the run discards completed work")
	}
	if stat.Added == 0 {
		t.Fatalf("added lines from a new file must be counted; got %+v", stat)
	}

	// The diff handed to the reviewer must show the new file too.
	diff, derr := runGit(wrapCtx, repoDir, wrap.Diff, diffArgs()...)
	if derr != nil {
		t.Fatalf("diff: %v", derr)
	}
	if !strings.Contains(diff, "added.go") {
		t.Fatalf("the reported diff must include newly created files; got:\n%s", diff)
	}

	// And it must actually reach the branch.
	job := CodingJob{HeadBranch: "onecamp-agent/newfile-1", Prompt: "add a file"}
	if res, ok := commitAndPush(wrapCtx, job, repoDir, wrap); !ok {
		t.Fatalf("a new-files-only change must be pushed: %+v", res)
	}
	bare := strings.TrimPrefix(originURL, "file://")
	if files := git(t, bare, "ls-tree", "--name-only", "onecamp-agent/newfile-1"); !strings.Contains(files, "added.go") {
		t.Fatalf("the new file must exist on the pushed branch; got %q", files)
	}
}

// TestIntegration_VerifierScratchIsNotCommitted proves the scratch directory a
// build gate writes into cannot end up in the pull request. Finalization stages
// with `git add -A`, so a TMPDIR inside the checkout meant residue from a gate —
// routine whenever a gate is killed at its deadline — was committed and published
// to GitHub alongside the real change.
func TestIntegration_VerifierScratchIsNotCommitted(t *testing.T) {
	requireGit(t)
	originURL, root := setupOrigin(t)
	repoDir := filepath.Join(root, "checkout")
	if out, err := runGit(context.Background(), "", 2*time.Minute, cloneArgs(originURL, "main", repoDir, 1, false)...); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	nb, err := newBranchArgs("onecamp-agent/scratch-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, nb...)

	// Simulate a gate leaving residue behind in its scratch dir, then a real edit.
	tmpDir := verifierTmpDir(repoDir)
	if mkErr := os.MkdirAll(tmpDir, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(tmpDir, "go-build-leftover.o"), []byte("junk"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	writeFile(t, repoDir, "main.go", "package main\n\nfunc main() { println(\"edited\") }\n")

	wrap := newWrapUpBudget(wrapUpReserve(15 * time.Minute))
	wrapCtx, cancel := context.WithTimeout(context.Background(), wrap.Total)
	defer cancel()
	if stat := computeDiffStat(wrapCtx, repoDir, wrap.Diff); stat.Files == 0 {
		t.Fatal("the real edit must still be seen")
	}
	job := CodingJob{HeadBranch: "onecamp-agent/scratch-1", Prompt: "edit main"}
	if res, ok := commitAndPush(wrapCtx, job, repoDir, wrap); !ok {
		t.Fatalf("push: %+v", res)
	}

	bare := strings.TrimPrefix(originURL, "file://")
	files := git(t, bare, "ls-tree", "-r", "--name-only", "onecamp-agent/scratch-1")
	if strings.Contains(files, "go-build-leftover.o") || strings.Contains(files, "verifier-tmp") || strings.Contains(files, ".tmp/") {
		t.Fatalf("build scratch must never be committed into the PR; tree was:\n%s", files)
	}
	if !strings.Contains(files, "main.go") {
		t.Fatalf("the real change must still be committed; tree was:\n%s", files)
	}
}

// TestIntegration_VerifierGateReapsOrphanedChildren proves a build or test gate
// cannot leave a process running after it finishes. exec.CommandContext kills only
// the direct child, so a script that backgrounds work (a compiler, a test server,
// a watcher — or something a malicious repo planted) used to outlive its gate and
// keep running for the rest of the job: burning the container's pid/CPU budget so
// a LATER gate fails for an unrelated reason, and staying alive across the push,
// where a git child briefly carries the repository credential in its argv.
//
// The gate here backgrounds a long sleep and exits immediately, which is the
// shape that used to escape: the gate itself succeeds, so no timeout kill runs.
func TestIntegration_VerifierGateReapsOrphanedChildren(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "orphan.pid")
	// Background a child that outlives the gate, record its pid, exit 0 at once.
	script := "sh -c 'sleep 300 & echo $! > " + marker + "' "
	gates := []verifierCmd{
		{Name: "build", Kind: verifierBuild, Argv: []string{"sh", "-c", script}},
	}
	report := runVerifiers(context.Background(), root, gates, 30*time.Second, 64<<10)
	if !report.AllPassed {
		t.Fatalf("the gate itself exits zero; got %+v", report)
	}

	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Skipf("could not capture the background pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Skipf("unusable background pid %q", raw)
	}

	// Give the sweep a moment, then the process must be gone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // reaped
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // don't leak it out of the test
	t.Fatalf("a gate must not leave process %d running after it finishes", pid)
}

// TestIntegration_ContinueBranch proves a follow-up adds a commit to the
// branch a pull request is open on, and that it never overwrites a push
// someone else made in the meantime.
func TestIntegration_ContinueBranch(t *testing.T) {
	requireGit(t)
	originURL, root := setupOrigin(t)
	bare := strings.TrimPrefix(originURL, "file://")
	head := "onecamp-agent/fix-1"

	// The first run: a fresh branch with one commit.
	first := filepath.Join(root, "first")
	git(t, "", cloneArgs(originURL, "main", first, 1, false)...)
	nb, _ := startBranchArgs(CodingJob{BaseBranch: "main", HeadBranch: head})
	git(t, first, nb...)
	if err := os.WriteFile(filepath.Join(first, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, _ := commitArgs("first change")
	for _, c := range cmds {
		git(t, first, c...)
	}
	push, _ := pushArgs(head)
	git(t, first, push...)

	// The follow-up: clone the head branch, start nothing, commit, push.
	job := CodingJob{BaseBranch: "main", HeadBranch: head, ContinueBranch: true}
	second := filepath.Join(root, "second")
	git(t, "", cloneArgs(originURL, checkoutBranch(job), second, 1, false)...)
	if nb, err := startBranchArgs(job); err != nil || nb != nil {
		t.Fatalf("continuing: %v %v", nb, err)
	}
	if err := os.WriteFile(filepath.Join(second, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, _ = commitArgs("follow-up change")
	for _, c := range cmds {
		git(t, second, c...)
	}
	git(t, second, push...)

	log := git(t, bare, "log", "--oneline", head)
	if !strings.Contains(log, "first change") || !strings.Contains(log, "follow-up change") {
		t.Fatalf("the branch must hold both commits:\n%s", log)
	}
	if strings.Contains(git(t, bare, "log", "--oneline", "main"), "change") {
		t.Fatal("main must be untouched")
	}

	// Someone else pushes to the branch after a third run has cloned it: that
	// run's push must be refused, and their commit must survive.
	third := filepath.Join(root, "third")
	git(t, "", cloneArgs(originURL, head, third, 1, false)...)
	other := filepath.Join(root, "other")
	git(t, "", cloneArgs(originURL, head, other, 1, false)...)
	if err := os.WriteFile(filepath.Join(other, "b.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "add", "-A")
	git(t, other, "commit", "-m", "their commit")
	git(t, other, "push", "origin", head)

	if err := os.WriteFile(filepath.Join(third, "c.txt"), []byte("ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmds, _ = commitArgs("our stale change")
	for _, c := range cmds {
		git(t, third, c...)
	}
	if out, err := runGit(context.Background(), third, 30*time.Second, push...); err == nil {
		t.Fatalf("a push on a branch that moved must be refused, not forced:\n%s", out)
	}
	if !strings.Contains(git(t, bare, "log", "--oneline", head), "their commit") {
		t.Fatal("their commit must survive")
	}
}
