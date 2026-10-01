package business

import (
	"context"
	"os"
	"strings"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
)

// A result with no steps must never read as passed.
//
// The naive "return false on the first failing step" loop returns TRUE for an
// empty slice, and every early return in Run produces exactly that: a result
// whose steps list stops short. A drill that reports PASSED because it never got
// far enough to test anything is the single worst outcome this code can have, so
// it is the first thing asserted.
func TestEmptyResultIsNotAPass(t *testing.T) {
	if allOK(nil) {
		t.Error("allOK(nil) reported a pass; a drill that ran no steps must never claim the install is sound")
	}
	if allOK([]StepResult{}) {
		t.Error("allOK on an empty slice reported a pass")
	}
}

func TestAllOKNeedsEveryStep(t *testing.T) {
	pass := []StepResult{{Name: "a", OK: true}, {Name: "b", OK: true}}
	if !allOK(pass) {
		t.Error("all steps OK but allOK said no")
	}
	for i := range pass {
		mixed := []StepResult{{Name: "a", OK: true}, {Name: "b", OK: true}}
		mixed[i].OK = false
		if allOK(mixed) {
			t.Errorf("step %d failed and allOK still reported a pass", i)
		}
	}
}

func TestDetailOnlyAppearsWhenSomethingIsWrong(t *testing.T) {
	if got := detailIf(false, "boom"); got != "" {
		t.Errorf("detail leaked onto a passing step: %q", got)
	}
	if got := detailIf(true, "boom"); got != "boom" {
		t.Errorf("detail lost on a failing step: %q", got)
	}
}

// The fixture channels must stay namespaced.
//
// The marketing story says "#finance", and the temptation to make the product
// match the story is exactly how a demo ends up writing into the channel a
// customer's finance team actually uses. A rename to a bare word is a decision
// worth stopping on.
func TestFixtureChannelsAreNamespaced(t *testing.T) {
	for _, name := range []string{AllowedChannel, ForbiddenChannel} {
		if !strings.HasPrefix(name, "drill-") {
			t.Errorf("fixture channel %q is not namespaced. Customers run this on a real "+
				"workspace; a bare name can collide with a channel they already use.", name)
		}
	}
	if AllowedChannel == ForbiddenChannel {
		t.Fatal("both fixture channels have the same name, so the drill tests nothing")
	}
}

// The intent must be recorded BEFORE the tool is fetched and run.
//
// This is the ordering guarantee the product sells, and it is invisible in the
// type system: any refactor that moves the audit write below the executor call
// still compiles, still passes every other test, and quietly turns "recorded
// before it ran" into "recorded if it happened to work". Asserted against the
// source because the alternative needs Postgres, Dgraph and Redis, and an
// invariant only checked in an environment nobody runs locally is not checked.
func TestIntentIsRecordedBeforeTheToolRuns(t *testing.T) {
	src := readRunBody(t)

	record := strings.Index(src, "auditActionAttempt")
	getExec := strings.Index(src, `GetExecutor("send_message")`)
	call := strings.Index(src, "exec(ctx,")

	if record < 0 || getExec < 0 || call < 0 {
		t.Fatalf("Run no longer looks like the drill it was (record=%d getExecutor=%d call=%d); "+
			"this guard is not watching what it thinks it is", record, getExec, call)
	}
	if record > getExec || record > call {
		t.Error("Run fetches or calls the executor before writing the attempt row. " +
			"The guarantee is that the decision is recorded first, so that an action which " +
			"cannot be recorded is never attempted.")
	}
}

// An attempt whose record failed must not proceed.
func TestRunAbandonsTheAttemptWhenTheRecordFails(t *testing.T) {
	src := readRunBody(t)

	record := strings.Index(src, "attemptErr := recordDrill")
	guard := strings.Index(src, "if attemptErr != nil {")
	call := strings.Index(src, "exec(ctx,")

	if record < 0 || guard < 0 || call < 0 {
		t.Fatalf("Run's audit-first shape is gone (record=%d guard=%d call=%d)", record, guard, call)
	}
	if !(record < guard && guard < call) {
		t.Error("the early return for a failed audit write no longer sits between the write and " +
			"the executor call, so an unrecordable action could still be attempted")
	}
	if !strings.Contains(src[guard:call], "return res, nil") {
		t.Error("the failed-audit branch no longer returns, so execution falls through to the attempt")
	}
}

// readRunBody returns the source of Run, so the ordering guards cannot be
// satisfied by text somewhere else in the file.
func readRunBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("governanceDrill.go")
	if err != nil {
		t.Fatalf("reading the drill source: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func Run(ctx context.Context")
	if start < 0 {
		t.Fatal("Run is gone from governanceDrill.go")
	}
	end := strings.Index(src[start+1:], "\nfunc ")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+1+end]
}

// A drill is the one agent action that is never unattended: an admin pressed
// the button. Its rows must say so, or the "nobody watching" filter finds the
// admin's own rehearsal among the runs a timer fired.
func TestTheDrillRunsAsSomebodyInTheRoom(t *testing.T) {
	got, ok := auditBusiness.InitiatorFromCtx(drillContext(context.Background()))
	if !ok || got != auditBusiness.InitiatorPerson {
		t.Fatalf("drill initiator = %q (set=%v), want %q", got, ok, auditBusiness.InitiatorPerson)
	}

	// And Run says so before it writes anything.
	src := readRunBody(t)
	set := strings.Index(src, "ctx = drillContext(ctx)")
	record := strings.Index(src, "recordDrill(")
	if set < 0 || record < 0 || set > record {
		t.Errorf("Run must put the initiator on the context before the first recordDrill (set=%d record=%d)", set, record)
	}
}
