package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFirstNonEmptyIsExact(t *testing.T) {
	if got := FirstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q, want third", got)
	}
	if got := FirstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty for no arguments", got)
	}
	if got := FirstNonEmpty("", "", ""); got != "" {
		t.Errorf("got %q, want empty when every value is empty", got)
	}
	// The distinction from FirstNonBlank, pinned: whitespace IS a value here.
	if got := FirstNonEmpty("", "  ", "x"); got != "  " {
		t.Errorf("got %q, want the whitespace value; callers choosing the exact variant "+
			"are relying on it not being skipped", got)
	}
}

func TestFirstNonBlankSkipsAndTrims(t *testing.T) {
	if got := FirstNonBlank("", "  ", "x", "y"); got != "x" {
		t.Errorf("got %q, want x", got)
	}
	if got := FirstNonBlank("", "   "); got != "" {
		t.Errorf("got %q, want empty when nothing is set", got)
	}
	// The half that four copies of this function got wrong: they skipped a blank
	// value and then returned the next one WITHOUT trimming it, so a GitHub client
	// secret read from an env file with a trailing newline was used with the
	// newline still in it.
	if got := FirstNonBlank("", " secret\n"); got != "secret" {
		t.Errorf("got %q, want the value trimmed; an untrimmed credential fails "+
			"authentication and names nothing", got)
	}
	if got := FirstNonBlank(); got != "" {
		t.Errorf("got %q, want empty for no arguments", got)
	}
}

// No fifteenth copy.
//
// This function existed FOURTEEN times under two names and three behaviours. The
// name promised one thing and did another depending on which package you were in,
// which is worse than not having it at all. A walk rather than a list, because the
// next copy will be written in a package nobody thought to add to a list.
func TestNobodyRedeclaresFirstNonEmpty(t *testing.T) {
	// Matches a local declaration of this shape under any of the names it has
	// worn, so renaming it is not a way around the rule.
	decl := regexp.MustCompile(`(?m)^func (?:first|First)(?:NonEmpty|NonBlank|Non)\w*\(`)

	roots := []string{"../business", "../controllers", "../domain", "../services", "../models"}
	var found []string
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this ratchet would silently pass: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if decl.Match(src) {
				found = append(found, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(found) > 0 {
		t.Errorf("a local first-non-empty helper came back in:\n  %s\n\n"+
			"Use helpers.FirstNonEmpty (exact) or helpers.FirstNonBlank (skips whitespace and "+
			"trims). Two names exist so a call site says which it means; a third local copy "+
			"means the name stops predicting the behaviour again.",
			strings.Join(found, "\n  "))
	}
}
