package codepr

import (
	"context"
	"strings"
	"testing"
)

// A run reports where it is, in order, so the person waiting sees progress
// instead of minutes of silence.
func TestRunReportsItsStagesInOrder(t *testing.T) {
	o, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	var seen []Stage
	o.OnStage = func(u StageUpdate) {
		seen = append(seen, u.Stage)
		if u.Stage != StageWorking && u.Result == nil {
			t.Fatalf("%s must carry the runner's result as evidence", u.Stage)
		}
	}
	if out := o.Run(context.Background(), task()); out.Status != StatusOK {
		t.Fatalf("run: %+v", out)
	}
	want := []Stage{StageWorking, StageChecking, StageOpening}
	if strings.Join(stageNames(seen), ",") != strings.Join(stageNames(want), ",") {
		t.Fatalf("stages %v, want %v", seen, want)
	}
}

// A run that stops early reports only the stages it reached.
func TestRunStopsReportingWhereItStops(t *testing.T) {
	res := okResult()
	res.Status = StatusNoGreen
	res.Verifier.AllPassed = false
	o, _ := baseOrch(&mockCodingRunner{Result: res})
	var seen []Stage
	o.OnStage = func(u StageUpdate) { seen = append(seen, u.Stage) }
	o.Run(context.Background(), task())
	if len(seen) != 1 || seen[0] != StageWorking {
		t.Fatalf("a run the verifier stopped must not claim to check scope or open a PR: %v", seen)
	}
	o2, _ := baseOrch(&mockCodingRunner{Result: okResult()})
	o2.Run(context.Background(), task()) // no listener: nothing breaks
}

func TestStageMessage(t *testing.T) {
	repo := RepoRef{Owner: "acme", Name: "web"}
	if m := StageMessage(StageUpdate{Stage: StageWorking, Repo: repo}); !strings.Contains(m, "acme/web") || !strings.Contains(m, "few minutes") {
		t.Fatalf("working: %q", m)
	}
	r := okResult()
	r.DiffStat = DiffStat{Files: 2, Added: 14, Removed: 3}
	r.Verifier = VerifierReport{Ran: []VerifierResult{{}}, AllPassed: true, HadTests: true}
	if m := StageMessage(StageUpdate{Stage: StageChecking, Repo: repo, Result: &r}); !strings.Contains(m, "2 files changed (+14 −3); the build and tests pass.") {
		t.Fatalf("checking must carry the evidence: %q", m)
	}
	r.Verifier.HadTests = false
	if m := StageMessage(StageUpdate{Stage: StageChecking, Result: &r}); !strings.Contains(m, "no tests") {
		t.Fatalf("a repo without tests must not be said to pass them: %q", m)
	}
	if m := StageMessage(StageUpdate{Stage: StageOpening}); !strings.Contains(m, "the repository") {
		t.Fatalf("no repo name falls back to words, not a bare slash: %q", m)
	}
	if StageMessage(StageUpdate{Stage: "unknown"}) != "" {
		t.Fatal("an unknown stage says nothing")
	}
}

func stageNames(s []Stage) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}
