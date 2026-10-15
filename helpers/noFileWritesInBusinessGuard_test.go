package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Business code writes no files of its own.
//
// WHY THIS EXISTS. Leaving a Google-synced event appended every guest's email
// address to /tmp/gcal_leave_debug.log: a debug trace left in, created
// world-readable and never rotated (calendar CAL-BUG-9). A log line goes to the
// log pipeline, which has retention and access rules; a file in /tmp has
// neither. Temporary files a job streams through (os.CreateTemp) are not
// caught here: they are private to the process and removed by their owner.
//
// A real need to write a file goes in fileWritesAllowed, with the reason.
func TestBusinessCodeWritesNoFiles(t *testing.T) {
	write := regexp.MustCompile(`\b(os\.WriteFile|os\.OpenFile|os\.Create|ioutil\.WriteFile)\(`)
	fileWritesAllowed := map[string]string{
		// "business/Example/file.go": "why it must write a file",
	}
	seen := 0
	err := filepath.WalkDir("../business", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		seen++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
		if fileWritesAllowed[rel] != "" {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if write.MatchString(line) {
				t.Errorf("%s:%d writes a file: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 100 {
		t.Fatalf("only %d business files were read; the walk is no longer looking at the code", seen)
	}
}
