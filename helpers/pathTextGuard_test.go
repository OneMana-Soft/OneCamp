package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A path parameter that carries words people type is read through PathText,
// never chi.URLParam. chi hands a parameter back still escaped whenever the
// request's escaping differs from Go's own, and encodeURIComponent's does for
// ":", "@", "&", "+" and "/": ⌘K searched "Q&A", "C++" and "from:@alice" as
// "Q%26A", "C%2B%2B" and "from%3A%40alice" and listed nothing (V-169).
//
// The words parameters: ⌘K's search text, an email, a slug (a booking page's,
// a room's or a published page's address), and a task's key as typed in the
// address bar (ref, task_ref). An id, a server-made token or link file, a
// time stamp or a fixed word (a provider, a kind) holds none of those
// characters, so chi.URLParam reads it as sent.
func TestWordsInAPathAreReadThroughPathText(t *testing.T) {
	words := regexp.MustCompile(`chi\.URLParam\([^,()]+,\s*"(search_text|email|slug|ref|task_ref)"\)`)
	var offenders []string
	read := 0
	for _, root := range []string{"../controllers", "../middleware", "../router", "../business"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			read++
			for i, line := range strings.Split(string(src), "\n") {
				if words.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1))
				}
			}
			return nil
		})
	}
	if read < 100 {
		t.Fatalf("read %d Go files: the guard is looking in the wrong place", read)
	}
	if len(offenders) > 0 {
		t.Errorf("%d path parameters that carry words are read with chi.URLParam, which hands them back still escaped; read them with helpers.PathText:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
