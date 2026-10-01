package helpers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// errStringInResponse matches an error STRING being put into a response body's DETAIL slot:
//
//	helpers.Envolope{"msg": "...", "err": err.Error()}
//	helpers.Envolope{"error": err.Error()}
//
// Both keys, because RedactErrorsInResponse is key-agnostic — it matches the error INTERFACE
// wherever it appears — so "error" is the same bypass as "err", and the two are used
// interchangeably across controllers (109 sites in 14 files use one or the other).
//
// Any receiver, because handlers name the variable err, cerr, derr, perr and so on.
//
// "msg" is DELIBERATELY EXCLUDED. See the note on scope below; it is the one key the client
// actually renders, so a string there can be legitimate and is in fact the prescribed fix.
var errStringInResponse = regexp.MustCompile(`"(err|error)":\s*[a-zA-Z_][a-zA-Z0-9_.]*\.Error\(\)`)

const errStringBaselinePath = "testdata/err-string-baseline.txt"

// The error-string-in-response pattern may not spread.
//
// WHY THIS EXISTS. RedactErrorsInResponse blanks the error INTERFACE at the WriteJSON chokepoint,
// and its comment used to claim that closed the class "permanently, including for code not yet
// written". It does not: `err.Error()` puts a STRING in the envelope, and a string is not an error,
// so it passes through. For a *pq.Error that string is the driver's message, which still names the
// constraint — less than the full struct the chokepoint was written to stop, and still more than a
// client needs.
//
// WHY A RATCHET RATHER THAN A FIX. The obvious fix is wrong. Blanking every string under err would
// delete messages that are the entire point of the response: "the stored API key can no longer be
// decrypted (usually because AI_CONFIG_KEK changed); re-enter the key in admin AI settings" is what
// turns a dead end into one field to correct. And deciding by message text — redacting anything containing "pq:" or "constraint" —
// fails in the direction that matters, because a wording or locale change makes it MISS and leak.
// That is the same argument that kept IsUniqueViolation off message matching.
//
// The real distinction is whether the SERVER AUTHORED the message for the reader, which a string
// cannot carry. It has to be expressed at the source, one site at a time, as a sentinel the handler
// recognises. So the migration is real work, and this holds the line meanwhile: 109 sites today,
// never 110, and never a fifteenth file.
//
// WHAT IT DOES NOT GUARD, AND WHY NOT. Only the detail keys err and error. `"msg": err.Error()`
// appears more often still, and is NOT checked here, because msg is the one key the client renders
// (lib/axiosInstance.ts falls back through data?.msg) and because putting a sentinel's message in
// msg is exactly the migration this check is meant to encourage. Ratcheting msg by shape would
// fail CI on the correct fix, and a check that punishes the fix gets deleted.
//
// That leaves a real gap, named rather than papered over: a raw dependency message in msg is
// rendered verbatim to the user. It is not shape-detectable, so it belongs to review, not to this
// test.
//
// It also fails when a count DROPS without the baseline being updated. That is deliberate: it makes
// progress explicit in the diff, and stops a file quietly climbing back up after someone reduced it.
func TestErrorStringsInResponsesDoNotGrow(t *testing.T) {
	root := "../controllers"
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("scan root %s missing — this check would silently pass: %v", root, err)
	}

	found := map[string]int{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if n := len(errStringInResponse.FindAll(raw, -1)); n > 0 {
			// Keyed the way the baseline records it, so the file is readable as a work list.
			found[strings.TrimPrefix(path, "../")] = n
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	baseline, err := readErrStringBaseline()
	if err != nil {
		t.Fatalf("reading %s: %v", errStringBaselinePath, err)
	}
	if len(found) == 0 {
		t.Fatalf("no occurrences found at all — the pattern this check looks for has changed shape "+
			"and it is now enforcing nothing. Verify %s before trusting a pass.", errStringBaselinePath)
	}

	var grown, shrunk, appeared, vanished []string
	for file, count := range found {
		allowed, listed := baseline[file]
		switch {
		case !listed:
			appeared = append(appeared, fmt.Sprintf("%s has %d", file, count))
		case count > allowed:
			grown = append(grown, fmt.Sprintf("%s has %d, baseline %d", file, count, allowed))
		case count < allowed:
			shrunk = append(shrunk, fmt.Sprintf("%s has %d, baseline %d", file, count, allowed))
		}
	}
	for file := range baseline {
		if _, still := found[file]; !still {
			vanished = append(vanished, file)
		}
	}
	sort.Strings(grown)
	sort.Strings(shrunk)
	sort.Strings(appeared)
	sort.Strings(vanished)

	if len(appeared) > 0 {
		t.Errorf("new file(s) putting an error string in a response (%d):\n  %s\n\n"+
			"Use a sentinel error and put its message in msg (see business/AI.ErrProviderKeyUnreadable), "+
			"or pass the error VALUE in err and let RedactErrorsInResponse blank it.",
			len(appeared), strings.Join(appeared, "\n  "))
	}
	if len(grown) > 0 {
		t.Errorf("more error strings than the baseline allows (%d):\n  %s",
			len(grown), strings.Join(grown, "\n  "))
	}
	if len(shrunk) > 0 || len(vanished) > 0 {
		t.Errorf("progress — lower these in %s so it cannot creep back up:\n  %s%s",
			errStringBaselinePath,
			strings.Join(append(shrunk, vanished...), "\n  "),
			"\n(a count that dropped, or a file now clean and removable)")
	}
}

// readErrStringBaseline parses "path count" lines, ignoring comments and blanks.
func readErrStringBaseline() (map[string]int, error) {
	raw, err := os.ReadFile(errStringBaselinePath)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"path count\", got %q", lineNo+1, line)
		}
		count, convErr := strconv.Atoi(fields[1])
		if convErr != nil {
			return nil, fmt.Errorf("line %d: %q is not a count: %w", lineNo+1, fields[1], convErr)
		}
		out[fields[0]] = count
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s parsed to nothing", errStringBaselinePath)
	}
	return out, nil
}
