package codepr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func TestRepoRef(t *testing.T) {
	r := RepoRef{Owner: "akashc777", Name: "OneCamp"}
	if r.FullName() != "akashc777/OneCamp" {
		t.Fatalf("FullName: %q", r.FullName())
	}
	if !r.Valid() {
		t.Fatal("expected valid repo ref")
	}
	if (RepoRef{Owner: "x"}).Valid() || (RepoRef{Name: "y"}).Valid() || (RepoRef{}).Valid() {
		t.Fatal("a repo ref missing owner or name must be invalid")
	}
}

func TestDefaultCodingLimits(t *testing.T) {
	l := defaultCodingLimits()
	if l.Wall <= 0 || l.CPU <= 0 || l.MemoryBytes <= 0 || l.PIDs <= 0 ||
		l.DiskBytes <= 0 || l.OutputBytes <= 0 || l.CloneTimeout <= 0 ||
		l.MaxVerifyIters <= 0 || l.MaxCloneDepth <= 0 {
		t.Fatalf("all default limits must be positive backstops: %+v", l)
	}
	if l.CPU > l.Wall {
		t.Fatal("CPU budget should not exceed wall-clock")
	}
	if l.CloneTimeout > l.Wall {
		t.Fatal("clone timeout should fit within the wall-clock budget")
	}
}

func TestMockCodingRunner_ReturnsPresetAndCapturesJob(t *testing.T) {
	m := &mockCodingRunner{Result: okResult()}
	job := CodingJob{
		ID:         "job-1",
		Repo:       RepoRef{Owner: "o", Name: "n"},
		CloneToken: "secret-token",
		BaseBranch: "main",
		HeadBranch: "onecamp-agent/fix",
		Limits:     defaultCodingLimits(),
	}
	res, err := m.Run(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusOK || !res.Verifier.AllPassed {
		t.Fatalf("expected ok+verified result, got %+v", res)
	}
	if m.Calls != 1 || m.LastJob.ID != "job-1" {
		t.Fatalf("runner did not capture the job: calls=%d job=%+v", m.Calls, m.LastJob)
	}
	// Invariant the orchestrator relies on: never push to the base branch.
	if m.LastJob.HeadBranch == m.LastJob.BaseBranch {
		t.Fatal("head branch must never equal the base branch")
	}
}

func TestMockCodingRunner_PropagatesError(t *testing.T) {
	m := &mockCodingRunner{Err: context.DeadlineExceeded}
	if _, err := m.Run(context.Background(), CodingJob{}); err == nil {
		t.Fatal("expected the preset error to propagate")
	}
}

func TestCodingJob_TokenNotSerialized(t *testing.T) {
	// The clone token must never be serialized (json:"-") so it can't leak into
	// a logged/audited job. Compile-time-ish guard via a marshal check.
	_ = time.Second
	j := CodingJob{CloneToken: "top-secret"}
	if got := mustJSON(j); contains(got, "top-secret") {
		t.Fatalf("clone token leaked into serialized job: %s", got)
	}
}

// TestCodingWallPrecedence pins the resolution order the whole platform depends
// on: the admin setting wins, then the env escape hatch, then the built-in
// default — always clamped, and always safe before any settings load.
func TestCodingWallPrecedence(t *testing.T) {
	t.Cleanup(func() { SetConfiguredCodingWall(0) })

	SetConfiguredCodingWall(0)
	if got := CodingWall(); got != DefaultCodingWallMinutes*time.Minute {
		t.Fatalf("unset must fall back to the built-in default: got %s", got)
	}

	t.Setenv(CodingWallEnvVar, "12")
	if got := CodingWall(); got != 12*time.Minute {
		t.Fatalf("env override must apply while the setting is unset: got %s", got)
	}

	if got := SetConfiguredCodingWall(45); got != 45*time.Minute {
		t.Fatalf("admin setting must win over the env override: got %s", got)
	}
	if got := CodingWall(); got != 45*time.Minute {
		t.Fatalf("configured wall not published: got %s", got)
	}

	if got := SetConfiguredCodingWall(0); got != 12*time.Minute {
		t.Fatalf("clearing the setting must fall back to the env override: got %s", got)
	}
}

// TestCodingWallClamping proves a typo can neither starve a run nor pin a worker,
// from either input path, and that MaxRunWallClock keeps tracking the result.
func TestCodingWallClamping(t *testing.T) {
	t.Cleanup(func() { SetConfiguredCodingWall(0) })

	if got := SetConfiguredCodingWall(1); got != MinCodingWallMinutes*time.Minute {
		t.Fatalf("below-range setting must clamp up: got %s", got)
	}
	if got := SetConfiguredCodingWall(10_000); got != MaxCodingWallMinutes*time.Minute {
		t.Fatalf("above-range setting must clamp down: got %s", got)
	}
	if want := MaxCodingWallMinutes*time.Minute + CodingDispatchOverhead; MaxRunWallClock() != want {
		t.Fatalf("MaxRunWallClock must derive from the configured wall: got %s want %s", MaxRunWallClock(), want)
	}
	if ClampCodingWallMinutes(0) != 0 || ClampCodingWallMinutes(-5) != 0 {
		t.Fatal("0 and negatives must pass through as unset")
	}
	if ClampCodingWallMinutes(30) != 30 {
		t.Fatal("an in-range value must pass through unchanged")
	}

	t.Setenv(CodingWallEnvVar, "999")
	SetConfiguredCodingWall(0)
	if got := CodingWall(); got != MaxCodingWallMinutes*time.Minute {
		t.Fatalf("out-of-range env override must clamp: got %s", got)
	}
}

// TestCodingLimitsWithWall proves the configured wall reaches the runner and
// keeps the inner budgets coherent (nothing may outlive the wall it sits in).
func TestCodingLimitsWithWall(t *testing.T) {
	l := defaultCodingLimits().withWall(3 * time.Minute)
	if l.Wall != 3*time.Minute {
		t.Fatalf("wall not applied: %s", l.Wall)
	}
	if l.CPU > l.Wall || l.CloneTimeout > l.Wall {
		t.Fatalf("inner budgets must fit inside the wall: %+v", l)
	}
	base := defaultCodingLimits()
	if got := base.withWall(0); got != base {
		t.Fatal("a non-positive wall must leave the limits untouched")
	}
	// Everything else is preserved — withWall only narrows timing.
	if l.MemoryBytes != base.MemoryBytes || l.MaxVerifyIters != base.MaxVerifyIters {
		t.Fatalf("withWall must not disturb non-timing limits: %+v", l)
	}
}
