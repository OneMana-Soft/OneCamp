package models

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every `for rows.Next()` loop must close its rows and check rows.Err().
//
// Both failures are silent, which is why they need a test rather than a habit.
//
// A missing Close leaks a pooled connection whenever the loop returns early —
// database/sql only auto-closes when Next() runs to completion, so the very path
// that handles a scan error is the one that leaks. Enough of those and the pool
// is exhausted and every query blocks; nothing in the logs points at the cause.
//
// A missing rows.Err() is worse because it corrupts results rather than
// resources. Iteration also stops when the query fails mid-flight — a dropped
// connection, a server-side error, a cancelled context — and without the check
// the function returns the rows it happened to get with a nil error. The caller
// cannot distinguish truncated data from a genuinely short list. This was live
// in the FCM token lookups, which run on every notification fan-out, so a
// mid-query failure silently dropped push notifications for whoever fell off the
// end of the list and reported success.
//
// Checked against source because there is no runtime seam: the property is about
// what the code does on a failure path that a unit test would have to simulate at
// the driver level.
var (
	rowsLoop  = regexp.MustCompile(`^\s*for rows\.Next\(\)\s*\{\s*$`)
	rowsClose = regexp.MustCompile(`rows\.Close\(\)`)
	rowsErr   = regexp.MustCompile(`rows\.Err\(\)`)
	funcDecl  = regexp.MustCompile(`^func `)
)

// modelSources walks the model tree from this package's directory.
func modelSources(t *testing.T) map[string][]string {
	t.Helper()
	sources := map[string][]string{}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), "for rows.Next()") {
			sources[path] = strings.Split(string(b), "\n")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("found no model files with rows loops; this test would pass vacuously")
	}
	return sources
}

// funcBounds returns the line range of the function enclosing idx.
func funcBounds(lines []string, idx int) (int, int) {
	start := 0
	for j := idx; j >= 0; j-- {
		if funcDecl.MatchString(lines[j]) {
			start = j
			break
		}
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		if funcDecl.MatchString(lines[j]) {
			end = j
			break
		}
	}
	return start, end
}

func TestEveryRowsLoopClosesAndChecksErr(t *testing.T) {
	loops := 0
	for path, lines := range modelSources(t) {
		for i, line := range lines {
			if !rowsLoop.MatchString(line) {
				continue
			}
			loops++
			start, end := funcBounds(lines, i)
			body := strings.Join(lines[start:end], "\n")

			if !rowsClose.MatchString(body) {
				t.Errorf("%s:%d: rows loop with no rows.Close() in the enclosing "+
					"function. An early return from the loop leaks the pooled "+
					"connection; database/sql only auto-closes when Next() completes.",
					path, i+1)
			}
			if !rowsErr.MatchString(body) {
				t.Errorf("%s:%d: rows loop with no rows.Err() check. Iteration also "+
					"stops when the query fails mid-flight, so without it the function "+
					"returns PARTIAL data with a nil error and the caller cannot tell.",
					path, i+1)
			}
		}
	}
	// Guards against the walk silently matching nothing after a directory move.
	if loops < 100 {
		t.Fatalf("only found %d rows loops; the audit expected ~123, so this test "+
			"is no longer looking where the code lives", loops)
	}
}
