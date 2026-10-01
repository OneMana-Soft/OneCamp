package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- git argv safety invariants (the most important guards) ---

func TestNewBranchArgs_RefusesBaseAndBadNames(t *testing.T) {
	if _, err := newBranchArgs("main", "main"); err == nil {
		t.Fatal("must refuse head == base")
	}
	if _, err := newBranchArgs("Main", "main"); err == nil {
		t.Fatal("must refuse head == base case-insensitively")
	}
	if _, err := newBranchArgs("", "main"); err == nil {
		t.Fatal("must refuse empty head")
	}
	if _, err := newBranchArgs("--upload-pack=x", "main"); err == nil {
		t.Fatal("must refuse flag-like head")
	}
	args, err := newBranchArgs("onecamp-agent/fix-1", "main")
	if err != nil || args[0] != "checkout" || args[1] != "-b" || args[2] != "onecamp-agent/fix-1" {
		t.Fatalf("valid branch args wrong: %v %v", args, err)
	}
}

func TestPushArgs_NeverForceOnlyHead(t *testing.T) {
	if _, err := pushArgs(""); err == nil {
		t.Fatal("must refuse empty head")
	}
	if _, err := pushArgs("--force"); err == nil {
		t.Fatal("must refuse flag-like head")
	}
	args, err := pushArgs("onecamp-agent/fix-1")
	if err != nil {
		t.Fatalf("push args error: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--force") || strings.Contains(joined, "-f") {
		t.Fatalf("push must never force: %q", joined)
	}
	if !strings.Contains(joined, "refs/heads/onecamp-agent/fix-1:refs/heads/onecamp-agent/fix-1") {
		t.Fatalf("push must target only the head branch: %q", joined)
	}
}

func TestCloneArgs_ShallowPartialSparse(t *testing.T) {
	args := cloneArgs("https://github.com/o/r.git", "main", "/work/repo", 0, true)
	j := strings.Join(args, " ")
	for _, must := range []string{"--depth 1", "--filter=blob:none", "--single-branch", "--branch main", "--no-checkout", "--sparse"} {
		if !strings.Contains(j, must) {
			t.Fatalf("clone args missing %q: %q", must, j)
		}
	}
}

func TestRepoCloneURL_CleanNoToken(t *testing.T) {
	u, err := repoCloneURL("github.com", "o", "r")
	if err != nil || u != "https://github.com/o/r.git" {
		t.Fatalf("bad clone url: %q %v", u, err)
	}
	// The clone URL must NEVER contain credentials (token would persist in
	// .git/config); auth is a separate per-command header.
	if strings.Contains(u, "@") || strings.Contains(u, "x-access-token") {
		t.Fatalf("clone URL must carry no credentials: %q", u)
	}
	if _, err := repoCloneURL("github.com", "", "r"); err == nil {
		t.Fatal("must require owner+name")
	}
}

func TestAuthHeaderConfigArgs(t *testing.T) {
	if got := authHeaderConfigArgs("  "); got != nil {
		t.Fatalf("empty token ⇒ nil args, got %v", got)
	}
	args := authHeaderConfigArgs("ghp_secret")
	if len(args) != 2 || args[0] != "-c" {
		t.Fatalf("expected a -c config pair, got %v", args)
	}
	// The raw token must not appear verbatim (it's base64'd inside a header);
	// and it must be an http.extraheader Authorization config.
	if !strings.HasPrefix(args[1], "http.extraheader=AUTHORIZATION: basic ") {
		t.Fatalf("unexpected auth config: %q", args[1])
	}
	if strings.Contains(args[1], "ghp_secret") {
		t.Fatalf("raw token must not appear in the config arg: %q", args[1])
	}
}

// --- edit protocol path safety ---

func TestSafeRepoPath_BlocksEscapes(t *testing.T) {
	root := "/work/coderun-x/repo"
	for _, bad := range []string{"/etc/passwd", "../outside", "a/../../b", ".git/config", ".git/hooks/pre-commit", ""} {
		if _, err := safeRepoPath(root, bad); err == nil {
			t.Fatalf("must reject unsafe path %q", bad)
		}
	}
	good, err := safeRepoPath(root, "src/app/main.go")
	if err != nil || !strings.HasPrefix(good, root+"/") {
		t.Fatalf("valid path rejected: %q %v", good, err)
	}
}

func TestParseEditResponse(t *testing.T) {
	resp, err := parseEditResponse(`{"edits":[{"path":"a.go","contents":"package a"}],"done":true}`)
	if err != nil || len(resp.Edits) != 1 || !resp.Done || resp.Edits[0].Path != "a.go" {
		t.Fatalf("bare object parse failed: %+v %v", resp, err)
	}
	fenced := "Sure:\n```json\n{\"edits\":[{\"path\":\"b.go\",\"delete\":true}]}\n```\n"
	resp, err = parseEditResponse(fenced)
	if err != nil || len(resp.Edits) != 1 || !resp.Edits[0].Delete {
		t.Fatalf("fenced parse failed: %+v %v", resp, err)
	}
	if _, err := parseEditResponse("no json here"); err == nil {
		t.Fatal("must error when no JSON object present")
	}
	// Empty-path edits are dropped.
	resp, _ = parseEditResponse(`{"edits":[{"path":"  ","contents":"x"},{"path":"c.go","contents":"y"}]}`)
	if len(resp.Edits) != 1 || resp.Edits[0].Path != "c.go" {
		t.Fatalf("empty-path edit not dropped: %+v", resp)
	}
}

// --- verifier detection ---

func TestDetectVerifiers(t *testing.T) {
	goGates := detectVerifiers(map[string]bool{"go.mod": true})
	if len(goGates) != 3 || goGates[0].Argv[0] != "go" {
		t.Fatalf("go detection wrong: %+v", goGates)
	}
	hasTest := false
	for _, g := range goGates {
		if isTestVerifier(g) {
			hasTest = true
		}
	}
	if !hasTest {
		t.Fatal("go gates must include a test gate")
	}
	node := detectVerifiers(map[string]bool{"package.json": true})
	if len(node) == 0 || node[0].Argv[0] != "npm" {
		t.Fatalf("node detection wrong: %+v", node)
	}
	if got := detectVerifiers(map[string]bool{"README.md": true}); got != nil {
		t.Fatalf("unknown repo must yield no gates, got %+v", got)
	}
}

func TestSummarizeOutput(t *testing.T) {
	out := "compiling...\nok\n./x.go:10: error: undefined: Foo\ndone"
	s := summarizeOutput(out, 20)
	if !strings.Contains(s, "error: undefined: Foo") {
		t.Fatalf("summary should surface the error line: %q", s)
	}
	// No error lines → falls back to the tail (non-empty).
	if s := summarizeOutput("line1\nline2\nline3", 2); s == "" {
		t.Fatal("summary of non-error output should be non-empty")
	}
}

func TestSanitizeRunner_StripsWorkPaths(t *testing.T) {
	if got := sanitizeRunner("failed at /work/coderun-abc/repo/x.go"); strings.Contains(got, "/work/") {
		t.Fatalf("must strip work paths: %q", got)
	}
}

// --- wrap-up timing model (never lose work to our own clock) ---

func TestWrapUpReserve_DerivedFromWallAndClamped(t *testing.T) {
	// The default wall: 20% of 15m = 3m, so the loop gets a 12m soft deadline and
	// finalization is always funded.
	if got := wrapUpReserve(15 * time.Minute); got != 3*time.Minute {
		t.Fatalf("15m wall should reserve 3m, got %s", got)
	}
	// The ceiling wall: the fraction (12m) is clamped, because finalization is
	// bounded work and must not eat editing time it cannot use.
	if got := wrapUpReserve(60 * time.Minute); got != maxWrapUpReserve {
		t.Fatalf("60m wall should clamp to %s, got %s", maxWrapUpReserve, got)
	}
	// The smallest allowed wall: the fraction (24s) is too small to push, so the
	// minimum applies — but it may never exceed half the wall.
	small := wrapUpReserve(2 * time.Minute)
	if small < minWrapUpReserve || small > time.Minute {
		t.Fatalf("2m wall should reserve the %s minimum, got %s", minWrapUpReserve, small)
	}
	if small > 2*time.Minute/2 {
		t.Fatalf("reserve must never exceed half the wall: %s", small)
	}
	// A missing wall still yields a usable reserve rather than zero.
	if got := wrapUpReserve(0); got != minWrapUpReserve {
		t.Fatalf("zero wall should fall back to %s, got %s", minWrapUpReserve, got)
	}
	// Every reserve leaves a positive soft window (soft + reserve == wall).
	for _, wall := range []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute} {
		if soft := wall - wrapUpReserve(wall); soft <= 0 {
			t.Fatalf("wall %s left no edit/verify time", wall)
		}
	}
}

func TestNewWrapUpBudget_FitsInsideTheReserve(t *testing.T) {
	reserve := wrapUpReserve(15 * time.Minute) // 3m
	b := newWrapUpBudget(reserve)
	if b.Total != reserve {
		t.Fatalf("budget total must be the reserve: %s vs %s", b.Total, reserve)
	}
	// The whole finalization sequence (numstat + diff, add + commit, push) must fit
	// in the reserve that funds it, so no step is promised time it cannot have.
	worst := 2*b.Diff + 2*b.Commit + b.Push
	if worst > b.Total {
		t.Fatalf("finalization steps (%s) exceed the reserve (%s)", worst, b.Total)
	}
	if b.Push < b.Diff || b.Push < b.Commit {
		t.Fatalf("the push (network) should get the largest share: %+v", b)
	}
	// Even a tiny reserve keeps every step runnable rather than zero.
	tiny := newWrapUpBudget(0)
	if tiny.Total <= 0 || tiny.Diff <= 0 || tiny.Commit <= 0 || tiny.Push <= 0 {
		t.Fatalf("a zero reserve must fall back to usable deadlines: %+v", tiny)
	}
}

func TestRoundHeadroom_PacesOnTheSlowestRound(t *testing.T) {
	// Before any round completes, one verify pass is the floor.
	if got := roundHeadroom(0); got != verifyGateTimeout {
		t.Fatalf("first-round headroom should be %s, got %s", verifyGateTimeout, got)
	}
	// A repo with slow gates earns proportionally more warning.
	if got := roundHeadroom(7 * time.Minute); got != 7*time.Minute {
		t.Fatalf("headroom should track the slowest round, got %s", got)
	}
}

func TestEditVerifyLoop_NeverStartsARoundPastTheSoftDeadline(t *testing.T) {
	job := CodingJob{Prompt: "do the thing", Limits: CodingLimits{MaxVerifyIters: 4}}
	llm := newLLMClient("", "", job.ID) // unconfigured: a started round would error
	// The soft deadline already passed (e.g. the clone ate the whole window).
	loop := editVerifyLoop(context.Background(), job, llm, t.TempDir(), time.Now().Add(-time.Second))
	report, iters, ranOutOfTime := loop.Report, loop.Iterations, loop.RanOutOfTime
	if iters != 0 {
		t.Fatalf("no round may start past the soft deadline, ran %d", iters)
	}
	if !ranOutOfTime {
		t.Fatal("the loop must report that the clock, not the work, stopped it")
	}
	if report.AllPassed {
		t.Fatal("a loop that never ran cannot report passing gates")
	}
}

func TestEditVerifyLoop_ModelUnavailableIsNotATimeout(t *testing.T) {
	job := CodingJob{Prompt: "do the thing", Limits: CodingLimits{MaxVerifyIters: 4}}
	llm := newLLMClient("", "", job.ID)
	loop := editVerifyLoop(context.Background(), job, llm, t.TempDir(), time.Now().Add(10*time.Minute))
	iters, ranOutOfTime := loop.Iterations, loop.RanOutOfTime
	if iters != 1 {
		t.Fatalf("an unreachable model should stop after one attempt, ran %d", iters)
	}
	if ranOutOfTime {
		t.Fatal("a model failure inside the deadline must not be reported as a timeout")
	}
}

func TestWrapUpInstruction_RidesTheExistingGuidanceChannel(t *testing.T) {
	// With nothing else to say, the wrap-up instruction is the whole guidance.
	only := appendWrapUpInstruction("")
	if only != wrapUpInstruction {
		t.Fatalf("wrap-up guidance should stand alone: %q", only)
	}
	// With a verifier failure pending, both survive — the model is told what broke
	// AND that this is the last round.
	prior := verifierFailureSummary(CodingVerifierReport{
		Ran: []CodingVerifierResult{{Name: "go test", Kind: verifierTest, Passed: false, Summary: "x_test.go:9: boom"}},
	})
	combined := appendWrapUpInstruction(prior)
	if !strings.Contains(combined, "boom") || !strings.Contains(combined, "FINAL round") {
		t.Fatalf("guidance must carry both the failure and the wrap-up: %q", combined)
	}
	// And it reaches the prompt through the SAME message builder.
	msgs := buildEditMessages("task", map[string]string{"a.go": "package a"}, combined)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user turns, got %d", len(msgs))
	}
	user := msgs[1].Content
	if !strings.Contains(user, "FINAL round") || !strings.Contains(user, "boom") || !strings.Contains(user, "task") {
		t.Fatalf("prompt missing task/failure/wrap-up: %q", user)
	}
}

func TestVerifierFailureSummary_IsSelfDescribing(t *testing.T) {
	// Sharing the guidance channel means the block must say what it is.
	s := verifierFailureSummary(CodingVerifierReport{
		Ran: []CodingVerifierResult{{Name: "go build", Kind: verifierBuild, Passed: false, Summary: "undefined: Foo"}},
	})
	if !strings.Contains(s, "did not pass verification") || !strings.Contains(s, "undefined: Foo") {
		t.Fatalf("summary should describe itself and the failure: %q", s)
	}
	// A passing report contributes no guidance.
	if s := verifierFailureSummary(CodingVerifierReport{AllPassed: true}); s != "" {
		t.Fatalf("a green report needs no guidance: %q", s)
	}
}

// --- HTTP deadline sizing (the admin can raise the wall without a redeploy) ---

func TestCodingWriteTimeout_SizedToTheWallCeiling(t *testing.T) {
	// The server sends the wall per job, so the sidecar sizes its write deadline to
	// the CEILING — a low local env default must not shorten it.
	t.Setenv(codingWallEnvVar, "5")
	got := codingWriteTimeout()
	want := maxCodingWall() + codingDispatchOverhead
	if got != want {
		t.Fatalf("write timeout should be the ceiling (%s), got %s", want, got)
	}
	if got <= codingWall()+codingDispatchOverhead {
		t.Fatalf("write timeout must exceed the local default's need: %s", got)
	}
	// An admin raising the setting to the ceiling still fits.
	t.Setenv(codingWallEnvVar, strconv.Itoa(maxCodingWallMinutes))
	if codingWriteTimeout() < codingWall() {
		t.Fatal("a maximum-length run must be able to answer before the deadline")
	}
	// Still bounded: a wedged connection is always reaped.
	if codingWriteTimeout() > 2*maxCodingWall() {
		t.Fatalf("write timeout must stay bounded, got %s", codingWriteTimeout())
	}
}
