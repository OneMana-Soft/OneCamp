package controllers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A request handler that panics on bad input answers 500 and writes a stack
// trace for what is the caller's mistake. Four handlers did this for a
// malformed time in the URL (found 4 Oct 2026); they now answer 400. This
// keeps it that way: no handler may panic on an error.
func TestHandlersDoNotPanicOnErrors(t *testing.T) {
	re := regexp.MustCompile(`\bpanic\(\s*err\s*\)`)
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d panics on an error; answer it instead (400 for bad input)", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
