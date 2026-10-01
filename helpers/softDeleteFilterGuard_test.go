package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Soft-deletion in Dgraph is a VALUE, never an absence.
//
// Every writer in this repository stores the Go zero time in *_deleted_at for a
// live row rather than omitting the predicate, so 0001-01-01T00:00:00Z is what
// "not deleted" looks like on disk and has(x_deleted_at) is true for every row
// that has ever been created. A filter written as `not has(x_deleted_at)`
// therefore matches NOTHING, and does it silently: the query succeeds, returns
// an empty set, and the caller reports whatever an empty set means for it.
//
// That is not hypothetical. domain/EntityLink used `not has(...)` in all seven of
// its filters while 158 queries elsewhere compared against the epoch. Every task
// and project answered "you are not a member" no matter who asked, linked docs
// and boards never appeared, and creating a link did nothing, for as long as the
// feature had existed.
//
// The correct form is `not gt(x_deleted_at, "1970-01-01T00:00:00Z")`: a real
// deletion is stamped with time.Now() and is comfortably greater than the epoch,
// and the zero time is not.
func TestSoftDeleteFiltersCompareValuesRatherThanExistence(t *testing.T) {
	// Matches the broken form in a Dgraph filter, including the ones built with
	// a %s placeholder for the predicate name.
	broken := regexp.MustCompile(`not has\(\s*(%s(_deleted_at)?|[a-z_]*_deleted_at)\s*\)`)

	root := ".."
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			// Skip vendored and build output rather than failing the walk.
			if info != nil && info.IsDir() {
				switch info.Name() {
				case "node_modules", ".git", "data", "vendor", ".next":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Comments may name the broken form in order to explain it.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if broken.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("soft-delete filters using existence instead of a value comparison "+
			"(these match nothing, silently) — use not gt(x, \"1970-01-01T00:00:00Z\"):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
