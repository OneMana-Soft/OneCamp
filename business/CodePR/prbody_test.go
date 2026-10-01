package codepr

import (
	"strings"
	"testing"
)

func TestBuildPRTitle(t *testing.T) {
	if got := BuildPRTitle("  fix the padding  "); got != "fix the padding" {
		t.Fatalf("trim/collapse failed: %q", got)
	}
	if got := BuildPRTitle(""); got != "Automated change" {
		t.Fatalf("empty task should fall back: %q", got)
	}
	long := strings.Repeat("word ", 40)
	if got := BuildPRTitle(long); len(got) > 74 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long title not truncated: %q (len %d)", got, len(got))
	}
	if got := BuildPRTitle("line one\nline two"); strings.Contains(got, "\n") {
		t.Fatalf("newlines must be stripped: %q", got)
	}
}

func TestBuildPRBody_VerifiedInScope(t *testing.T) {
	body := BuildPRBody(PRProvenance{
		AgentName:   "Vella",
		RequestedBy: "Akash",
		Task:        "fix the double padding",
		DiffStat:    DiffStat{Files: 1, Added: 3, Removed: 3},
		Verifier: VerifierReport{
			AllPassed: true, HadTests: true,
			Ran: []VerifierResult{{Name: "go test", Kind: VerifierTest, Passed: true}},
		},
		Verdict: Verdict{InScope: true},
	})
	for _, must := range []string{"Vella", "Akash", "fix the double padding", "✅ go test", "awaiting human review"} {
		if !strings.Contains(body, must) {
			t.Fatalf("body missing %q:\n%s", must, body)
		}
	}
	// In-scope, verified, non-draft => no scope-review or draft warnings.
	if strings.Contains(body, "Scope review") || strings.Contains(body, "draft") {
		t.Fatalf("clean run should carry no warnings:\n%s", body)
	}
}

func TestBuildPRBody_HonestWarnings(t *testing.T) {
	body := BuildPRBody(PRProvenance{
		AgentName: "Vella",
		Task:      "migrate the thing",
		DiffStat:  DiffStat{Files: 9, Added: 100, Removed: 20, PartialScope: true},
		Verifier: VerifierReport{
			AllPassed: false, HadTests: false,
			Ran: []VerifierResult{{Name: "go build", Kind: VerifierBuild, Passed: false}},
		},
		Verdict:        Verdict{InScope: false, Concern: "also disables an unrelated test"},
		Draft:          true,
		WholeRepo:      true,
		CandidateCount: 12,
	})
	for _, must := range []string{
		"❌ go build",
		"No tests were present",
		"Partial scope",
		"draft",
		"Scope review",
		"disables an unrelated test",
		"12 candidate file(s)",
	} {
		if !strings.Contains(body, must) {
			t.Fatalf("body missing honest warning %q:\n%s", must, body)
		}
	}
}

func TestBuildPRBody_NoVerifiers(t *testing.T) {
	body := BuildPRBody(PRProvenance{AgentName: "A", Task: "t", DiffStat: DiffStat{Files: 1}})
	if !strings.Contains(body, "No verifiers ran") {
		t.Fatalf("should disclose no verifiers:\n%s", body)
	}
}

func TestBuildHeadBranch(t *testing.T) {
	b := BuildHeadBranch("Fix the double padding!", "a3a5bc9")
	if !strings.HasPrefix(b, "onecamp-agent/") {
		t.Fatalf("must be prefixed: %q", b)
	}
	if strings.ContainsAny(b, " !") || strings.Contains(b, "--") {
		t.Fatalf("branch not clean: %q", b)
	}
	// Uniqueness suffix is applied.
	if !strings.HasSuffix(b, "-a3a5bc9") {
		t.Fatalf("suffix missing: %q", b)
	}
	// Empty task still yields a safe branch.
	if got := BuildHeadBranch("", ""); !strings.HasPrefix(got, "onecamp-agent/change-") {
		t.Fatalf("empty task fallback failed: %q", got)
	}
	// A fresh branch is never a bare base name.
	if BuildHeadBranch("main", "x") == "main" {
		t.Fatal("head branch must never equal a base branch name")
	}
}
