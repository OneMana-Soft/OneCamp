package helpers

import (
	"os"
	"strings"
	"testing"
)

// The Makefile customers get must name the frontend branch, and derive it.
//
// WHAT WENT WRONG. The frontend repository has one branch per edition and its
// DEFAULT is main, the AI-free one. Every place we told a customer how to deploy
// the web app printed a bare `git clone`, so a v2 operator deployed a frontend
// with no AI surface at all — no agents, no AI admin tab, no governance drill —
// against a backend that has every one of them. Nothing failed. The features
// simply were not there, which from the outside reads as "the AI edition does not
// work" and is close to impossible to diagnose without knowing the repository has
// branches at all.
//
// Derived from version.txt, which the archive writes with the release that
// produced it, so the printed command cannot disagree with what is installed.
func TestDistributeMakefileClonesTheRightFrontendBranch(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading Makefile-distribute: %v", err)
	}
	mk := string(raw)

	if !strings.Contains(mk, "git clone -b $(FE_BRANCH)") {
		t.Error("the deploy instructions do not clone a named branch, so a v2 install is " +
			"pointed at the AI-free frontend")
	}
	if strings.Contains(mk, "git clone https://github.com/OneMana-Soft/OneCamp-fe") {
		t.Error("a bare git clone is still printed; without -b it takes the default branch, " +
			"which is the AI-free one whatever edition is installed")
	}
	if !strings.Contains(mk, "FE_BRANCH :=") {
		t.Fatal("FE_BRANCH is not defined")
	}
	// Derived, not hardcoded: a constant would be wrong for one of the two editions.
	decl := mk[strings.Index(mk, "FE_BRANCH :="):]
	decl = decl[:strings.IndexByte(decl, '\n')]
	if !strings.Contains(decl, "version.txt") {
		t.Errorf("FE_BRANCH is not derived from the installed version (%q). A fixed value is "+
			"wrong for one of the two editions, and nothing would catch it.", decl)
	}
	for _, branch := range []string{"ai", "main"} {
		if !strings.Contains(decl, branch) {
			t.Errorf("FE_BRANCH cannot produce %q, so one edition has no correct answer", branch)
		}
	}
}
