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

	// A fixed delay standing in for readiness, anywhere before the verify call.
	restore := recipeOf(t, mk, "restore")
	fixedWait := regexp.MustCompile(`sleep [0-9]+; \\\t\$\(MAKE\)[^;]*verify`)
	if fixedWait.MatchString(restore) {
		t.Error("restore still sleeps a fixed number of seconds and then runs verify. " +
			"That judges a stack that may still be starting, and reports NOT READY on a " +
			"restore that worked.")
	}

	// Every target that starts the stack and then verifies it waits first, with
	// the one shared wait: install and update had none, and an update that worked
	// ended in NOT READY with the API still starting.
	for _, target := range []string{"restore", "install_run", "update_apply"} {
		r := recipeOf(t, mk, target)
		wait, verify := strings.Index(r, "--no-print-directory wait-ready"), strings.Index(r, "--no-print-directory verify")
		if verify < 0 {
			t.Errorf("%s no longer runs verify; this guard is watching nothing there", target)
			continue
		}
		if wait < 0 || wait > verify {
			t.Errorf("%s verifies without running wait-ready first, so it judges a stack that may still be starting", target)
		}
	}

	// The wait polls, against the same list verify uses, or the two can disagree
	// about what ready means; still starting counts as not ready, because Traefik
	// routes a container only once its health check passes.
	wait := recipeOf(t, mk, "wait-ready")
	if !strings.Contains(recipeOf(t, mk, "verify"), "$$($(stack_services))") || !strings.Contains(mk, "stack_services = $(COMPOSE) config --services") {
		t.Error("verify and wait-ready no longer share stack_services, so they can disagree about what ready means")
	}
	for _, want := range []string{
		"$$($(stack_services))",
		"while [ $$waited -lt $(WAIT_READY_SECONDS) ]",
		"health=unhealthy",
		"health=starting",
		"good -ge 2",
	} {
		if !strings.Contains(wait, want) {
			t.Errorf("wait-ready does not wait for readiness: %q is missing. Without it the "+
				"verify that follows is timing-dependent.", want)
		}
	}

	// The wait must be bounded, or a stack that never comes up hangs the restore
	// instead of reporting what is wrong.
	if !strings.Contains(wait, "still starting after") {
		t.Error("the readiness wait has no timeout message, so a stack that never starts " +
			"leaves the operator watching a silent command rather than reading a warning")
	}
}

// recipeOf returns a target's rule, from its name to the first blank line.
func recipeOf(t *testing.T, mk, target string) string {
	t.Helper()
	start := strings.Index(mk, "\n"+target+":")
	if start < 0 {
		t.Fatalf("no %s target in Makefile-distribute; this guard is watching nothing", target)
	}
	end := strings.Index(mk[start+1:], "\n\n")
	if end < 0 {
		end = len(mk) - start - 1
	}
	return mk[start : start+1+end]
}

// restore puts the graph and the files back through containers, never as the
// operator. Dgraph and MinIO run as root, so their data is root's, and an
// operator who is not root could not write there: rsync failed on the first
// file, and set -e ended the restore with the database already replaced. Only
// the chmod 777 that every rebuild used to do hid it, and never on a machine just
// installed, which is where make move-in restores.
func TestRestorePutsDataBackThroughContainers(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	r := recipeOf(t, string(raw), "restore")
	for _, banned := range []string{"rsync -", "-C ./data/dgraph", "mkdir -p ./data/dgraph"} {
		if strings.Contains(r, banned) {
			t.Errorf("restore writes into a datastore's directory as the operator (%q); a non-root operator can't, after the data dirs got their owners", banned)
		}
	}
	for _, want := range []string{
		"$(COMPOSE) exec -T alpha sh -c 'rm -rf /dgraph/restore && mkdir -p /dgraph/restore && tar -xzf - -C /dgraph/restore'",
		"docker run --rm --network none",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("restore no longer puts data back through a container: %q is missing", want)
		}
	}
}

// verify asks storage-check, so an access key that can't open its bucket is a
// FAIL in the check every install, update and restore ends with, not a silent
// "ok minio" from the API's startup log, which connects without asking MinIO
// anything.
func TestVerifyAsksStorageCheck(t *testing.T) {
	raw, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(raw)
	v := recipeOf(t, mk, "verify")
	call := strings.Index(v, "--no-print-directory storage-check")
	if call < 0 {
		t.Fatal("verify doesn't run storage-check")
	}
	if !strings.Contains(v[call:], "*FAIL*) fail=1") {
		t.Error("verify runs storage-check but doesn't fail when it reports FAIL")
	}
	recipeOf(t, mk, "storage-check")
}
