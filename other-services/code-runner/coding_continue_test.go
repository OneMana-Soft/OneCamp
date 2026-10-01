package main

import (
	"strings"
	"testing"
)

func TestContinuingABranchClonesItAndStartsNoNewOne(t *testing.T) {
	fresh := CodingJob{BaseBranch: "main", HeadBranch: "onecamp-agent/fix-1"}
	if b := checkoutBranch(fresh); b != "main" {
		t.Fatalf("a fresh run clones the base, got %q", b)
	}
	if nb, err := startBranchArgs(fresh); err != nil || len(nb) != 3 || nb[0] != "checkout" || nb[1] != "-b" {
		t.Fatalf("a fresh run creates its branch: %v %v", nb, err)
	}

	cont := fresh
	cont.ContinueBranch = true
	if b := checkoutBranch(cont); b != "onecamp-agent/fix-1" {
		t.Fatalf("a continuing run clones the head branch, got %q", b)
	}
	if nb, err := startBranchArgs(cont); err != nil || nb != nil {
		t.Fatalf("a continuing run is already on its branch: %v %v", nb, err)
	}

	for _, bad := range []CodingJob{
		{BaseBranch: "main", HeadBranch: "main", ContinueBranch: true},
		{BaseBranch: "main", HeadBranch: "", ContinueBranch: true},
		{BaseBranch: "main", HeadBranch: "--upload-pack=x", ContinueBranch: true},
	} {
		if _, err := startBranchArgs(bad); err == nil {
			t.Fatalf("continuing must still refuse %+v", bad)
		}
	}
}

func TestPushFailureMessage_NamesAMovedBranchWhenContinuing(t *testing.T) {
	if got := pushFailureMessage(CodingJob{ContinueBranch: true}); !strings.Contains(got, "did not push over their commits") {
		t.Fatalf("continuing push failure = %q", got)
	}
	if got := pushFailureMessage(CodingJob{}); got != "could not push the branch to the repository." {
		t.Fatalf("fresh push failure = %q", got)
	}
}
