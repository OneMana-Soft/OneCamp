package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A restore must WAIT for the stack, not sleep for a guess and then judge it.
//
// WHAT WENT WRONG. `restore` ended with `sleep 8` and then ran `verify`. Eight
// seconds is not how long a stack takes to come back: the application connects to
// Postgres, Dgraph, Redis, MinIO, MQTT and OpenSearch before it reports running.
// So verify judged a stack that was still starting and printed
//
//	FAIL  not running: go-service
//	NOT READY — fix the FAIL lines above
//
// on a restore that had worked perfectly. The demo host logged exactly that every
// night for seventeen nights, and a customer meets it at the one moment they are
// least able to tell a real failure from a false one — straight after restoring a
// backup, which is already the worst day of their month.
//
// A check that cries wolf stops being read, which is the same failure as an audit
// log that always reports tampering: the alarm is worse than no alarm because it
// trains the reader to ignore the real one.
func TestRestoreWaitsForReadinessInsteadOfSleeping(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading Makefile-distribute: %v", err)
	}
	mk := string(raw)

	start := strings.Index(mk, "\nrestore:")
	if start < 0 {
		t.Fatal("no restore target in Makefile-distribute; this guard is watching nothing")
	}
	end := strings.Index(mk[start+1:], "\n\n")
	if end < 0 {
		end = len(mk) - start - 1
	}
	recipe := mk[start : start+1+end]

	// A fixed delay standing in for readiness, anywhere before the verify call.
	fixedWait := regexp.MustCompile(`sleep [0-9]+; \\t\$\(MAKE\)[^;]*verify`)
	if fixedWait.MatchString(recipe) {
		t.Error("restore still sleeps a fixed number of seconds and then runs verify. " +
			"That judges a stack that may still be starting, and reports NOT READY on a " +
			"restore that worked.")
	}

	// And it must actually poll, against the same list verify uses, or the two can
	// disagree about what ready means.
	for _, want := range []string{
		"$(COMPOSE) config --services",
		"while [ $$waited -lt",
		"health=unhealthy",
		"good -ge 2",
	} {
		if !strings.Contains(recipe, want) {
			t.Errorf("restore does not wait for readiness: %q is missing. Without it the "+
				"verify that follows is timing-dependent.", want)
		}
	}

	// The wait must be bounded, or a stack that never comes up hangs the restore
	// instead of reporting what is wrong.
	if !strings.Contains(recipe, "still starting after") {
		t.Error("the readiness wait has no timeout message, so a stack that never starts " +
			"leaves the operator watching a silent command rather than reading a warning")
	}
}
