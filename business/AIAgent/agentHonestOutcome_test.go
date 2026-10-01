package business

import (
	"testing"

	codepr "github.com/akashc777/OneCamp/business/CodePR"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// On the demo a coding run failed (the runner could not write its work folder)
// while the thread said "Done — completed the requested changes. Worked with:
// code pr." Starting a background job is not finishing the work.
func TestLandedWorkFallbackOnlyClaimsWhatFinished(t *testing.T) {
	if got := landedWorkFallback([]string{"code_pr"}); got == "Done — completed the requested changes." {
		t.Fatalf("a run whose only write started a coding job claimed it was done: %q", got)
	}
	if got := landedWorkFallback([]string{"search_workspace", "code_pr"}); got != "Started — I'll post the result here when it's ready." {
		t.Fatalf("reads plus a started job: %q", got)
	}
	if got := landedWorkFallback([]string{"create_task"}); got != "Done — completed the requested changes." {
		t.Fatalf("a write that finished should still read as done: %q", got)
	}
	if got := landedWorkFallback([]string{"code_pr", "create_task"}); got != "Done — completed the requested changes." {
		t.Fatalf("a finished write alongside a started job: %q", got)
	}
}

// A coding run's job is done only when a pull request opened. Everything else
// is failed, which is as terminal as done, so it never retries into a second PR.
func TestCodePRTerminalState(t *testing.T) {
	if s, e := codePRTerminalState(codepr.Outcome{Status: codepr.StatusOK}); s != model.TaskDone || e != "" {
		t.Fatalf("ok: %q %q", s, e)
	}
	for _, st := range []string{codepr.StatusNoGreen, codepr.StatusTooLarge, codepr.StatusTimeout, codepr.StatusError, codepr.StatusUnavailable} {
		s, e := codePRTerminalState(codepr.Outcome{Status: st, StopReason: "budget"})
		if s != model.TaskFailed || e != "code_pr "+st+": budget" {
			t.Fatalf("%s: %q %q", st, s, e)
		}
	}
}
