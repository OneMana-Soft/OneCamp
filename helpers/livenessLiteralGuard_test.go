package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// epochLiteralBaseline is how many times the server's source spells the
// liveness boundary "1970-01-01T00:00:00Z" itself rather than through
// dgraphquery.Live or Deleted (208 on beta at the Dgraph plan, 18 Oct 2026;
// 209 on F1's base, 176 once domain/User moved onto Live; 162 once the
// projects chain landed: its one task-list filter builder took the board's and
// the lists' four due-date literals onto Deleted, and every read it adds or
// rewrites spells it through Live or Deleted, among them ten of
// domain/Project's that gained the restricted-task mark; 161 with L-1, whose
// two reads of a project's places, GetDgraphProjectTaskPlaces and
// GetDgraphProjectRanks, spell it through Live). It only goes down: move a
// filter onto Live or Deleted, then lower this to the count the test reports.
// without-ai's own count, without the AI edition's packages (161 on main).
const epochLiteralBaseline = 158

// The liveness boundary is spelled in one place.
//
// WHY. A live node's deletion time is 0001-01-01 and a deleted one's is after
// the epoch, so "is it live" is not gt(x_deleted_at, epoch). Written out by
// hand in some 200 filters, it is one typo (or one has(x_deleted_at)) from a
// filter that hides everything: on 26 Sep 2026 one hid every doc from every
// AI answer. dgraphquery.Live and Deleted write it once. This counts the
// literals in the server's non-test Go (helpers/dgraphquery/live.go, which
// defines it, aside) and fails when there are more than epochLiteralBaseline,
// or fewer (lower it with the change that removed them).
func TestTheLivenessBoundaryIsSpelledInFewerPlaces(t *testing.T) {
	count, files := 0, 0
	for _, root := range []string{"../business", "../controllers", "../domain", "../models", "../services", "../helpers", "../middleware", "../router", "../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			if filepath.ToSlash(path) == "../helpers/dgraphquery/live.go" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			count += strings.Count(string(b), "1970-01-01T00:00:00Z")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the source", files)
	}
	switch {
	case count > epochLiteralBaseline:
		t.Errorf("the source spells \"1970-01-01T00:00:00Z\" %d times, more than the %d allowed: write the filter with dgraphquery.Live or Deleted", count, epochLiteralBaseline)
	case count < epochLiteralBaseline:
		t.Errorf("the source spells \"1970-01-01T00:00:00Z\" %d times now, fewer than %d: lower epochLiteralBaseline to %d", count, epochLiteralBaseline, count)
	}
}
