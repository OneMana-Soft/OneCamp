package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `make update` must not report success for doing nothing.
//
// WHAT WENT WRONG. version.txt is written by the archive, so unzipping a release
// changes it before a single container is rebuilt: it is the STAGED version, and
// nothing anywhere recorded which release the running containers were built from.
// So a customer who read "UPGRADING: make update" in a release note and ran
// exactly that, without first re-running the installer that stages a new zip, got
// a backup, a migration pass with nothing to apply, a rebuild of identical
// source, and the word "Updated." while still on the old release.
//
// The fix is a marker written only after every step succeeds, and an early return
// when the staged version is the one already applied.
//
// DRIVEN, NOT READ. The first attempt wrote the early return as an `exit 0` in a
// recipe line, which ends that LINE's shell and leaves make to run the next one,
// so it printed its message and then backed up and rebuilt anyway. A test that
// grepped for the message would have passed on that version. This one runs the
// target in a scratch directory and looks at what it actually did.

// runUpdate runs `make update` in a scratch copy of the customer Makefile, with
// the two version files set as given ("" means the file is absent).
func runUpdate(t *testing.T, staged, applied string, extraArgs ...string) string {
	t.Helper()

	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading Makefile-distribute: %v", err)
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if body == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("Makefile", string(mk))
	// .env must exist or update refuses before it reaches the decision.
	write(".env", "DB_USER=x")
	write("version.txt", staged)
	write(".applied_version", applied)

	args := append([]string{"update"}, extraArgs...)
	cmd := exec.Command("make", args...)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput() // a non-zero exit is expected once it reaches docker
	return string(out)
}

// reachedTheWork reports whether the target got past the decision and started
// doing things. In a scratch directory with no stack, the first real step is the
// postgres precondition, so that message is the signal.
func reachedTheWork(out string) bool {
	return strings.Contains(out, "postgres is not running") ||
		strings.Contains(out, "backing up before anything changes")
}

func TestUpdateStopsWhenNothingNewIsStaged(t *testing.T) {
	out := runUpdate(t, "v2.11.0", "v2.11.0")

	if reachedTheWork(out) {
		t.Errorf("update did the work with nothing new staged:\n%s", out)
	}
	if !strings.Contains(out, "Already running v2.11.0") {
		t.Errorf("update did not say the release was already applied:\n%s", out)
	}
	// The way to actually get a newer release, named where the question is asked.
	if !strings.Contains(out, "install command from your licence") {
		t.Errorf("update did not say how to obtain a newer release:\n%s", out)
	}
	if strings.Contains(out, "Updated") {
		t.Errorf("update claimed an update happened:\n%s", out)
	}
}

func TestUpdateProceedsWhenANewerReleaseIsStaged(t *testing.T) {
	out := runUpdate(t, "v2.12.0", "v2.11.0")

	if !reachedTheWork(out) {
		t.Errorf("update refused to apply a staged release:\n%s", out)
	}
	if !strings.Contains(out, "v2.11.0 -> v2.12.0") {
		t.Errorf("update did not name what it was applying:\n%s", out)
	}
}

// Every install that predates the marker has no record of what it is running.
// Refusing those would be a worse failure than the one being fixed.
func TestUpdateProceedsWhenNothingWasEverRecorded(t *testing.T) {
	out := runUpdate(t, "v2.11.0", "")

	if !reachedTheWork(out) {
		t.Errorf("update refused on an install with no applied-version marker:\n%s", out)
	}
}

// An install with NEITHER file is the oldest case there is: no marker, and a
// version.txt that never arrived or was lost. Both readings of "unknown" are
// empty strings, so a guard that compares them without checking either is present
// concludes they match and refuses to update, permanently, on exactly the installs
// most in need of one.
func TestUpdateProceedsWhenNeitherVersionIsKnown(t *testing.T) {
	out := runUpdate(t, "", "")

	if !reachedTheWork(out) {
		t.Errorf("update refused on an install with no version files at all:\n%s", out)
	}
}

// Rebuilding the same release is legitimate — after an .env edit, say — and has
// to stay reachable once the no-op guard exists.
func TestUpdateRebuildsTheSameReleaseWhenAsked(t *testing.T) {
	out := runUpdate(t, "v2.11.0", "v2.11.0", "REBUILD=1")

	if !reachedTheWork(out) {
		t.Errorf("REBUILD=1 did not get past the no-op guard:\n%s", out)
	}
}

// runVerify runs `make verify` in a scratch copy and returns its output. Only the
// release section can be judged without a stack; everything after it fails on the
// missing containers, which is fine because the release line prints first.
func runVerify(t *testing.T, staged, applied string) string {
	t.Helper()

	mk, err := os.ReadFile("../Makefile-distribute")
	if err != nil {
		t.Fatalf("reading Makefile-distribute: %v", err)
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if body == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("Makefile", string(mk))
	write(".env", "DB_USER=x")
	write("version.txt", staged)
	write(".applied_version", applied)

	cmd := exec.Command("make", "verify")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// The first question about any broken install is which version it is, and the box
// could not answer: version.txt says what was UNPACKED, which is the new release
// from the moment a zip lands and before anything has been built onto it.
func TestVerifyReportsTheRunningRelease(t *testing.T) {
	out := runVerify(t, "v2.11.0", "v2.11.0")
	if !strings.Contains(out, "running v2.11.0") {
		t.Errorf("verify does not report the running release:\n%s", out)
	}
}

// A release unpacked and never applied is the state the installer leaves behind
// when an operator declines "apply it now", and it is invisible from the outside:
// every health check is green and the workspace is on the old code.
func TestVerifyFlagsAReleaseThatWasNeverApplied(t *testing.T) {
	out := runVerify(t, "v2.12.0", "v2.11.0")
	if !strings.Contains(out, "TODO") || !strings.Contains(out, "not applied") {
		t.Errorf("verify did not flag an unapplied release:\n%s", out)
	}
	if !strings.Contains(out, "make update") {
		t.Errorf("verify flagged it without saying how to apply it:\n%s", out)
	}
}
