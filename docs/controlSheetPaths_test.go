package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every file this document points at must exist.
//
// The whole value of the control sheet is that a buyer can check the claim
// themselves: each row names the file that proves it. A path that rots turns the
// strongest thing about the document into its weakest, because a reader who
// follows one reference to nothing stops trusting the rest of them, and they are
// right to.
//
// Paths rot silently. A rename that updates every import and every test leaves a
// markdown table pointing at a file that no longer exists, and nothing anywhere
// notices. This notices.
func TestControlSheetPathsExist(t *testing.T) {
	repoRoot := ".."

	// Some of these are edition-specific. AgentControls.md describes agent
	// controls and is absent from the AI-free line on purpose, because an edition
	// that ships no agents must not hand a buyer a sheet claiming approval gates
	// and per-agent budgets it does not have. A document that is correctly absent
	// is not a failure; a document that is present and lies is.
	docs := []string{"AgentControls.md", "AIActControls.md"}
	found := 0

	for _, doc := range docs {
		body, readErr := os.ReadFile(doc)
		if os.IsNotExist(readErr) {
			continue
		}
		found++
		t.Run(doc, func(t *testing.T) {
			if readErr != nil {
				t.Fatalf("could not read %s: %v", doc, readErr)
			}

			// Backtick-quoted things that look like a repo path: they contain a
			// slash and end in a known source extension or a directory slash.
			refs := regexp.MustCompile("`([A-Za-z0-9_./-]+/[A-Za-z0-9_./-]*)`").FindAllStringSubmatch(string(body), -1)
			if len(refs) == 0 {
				t.Fatalf("%s cites no files at all, which means it stopped being checkable", doc)
			}

			seen := map[string]bool{}
			checked := 0
			for _, m := range refs {
				ref := m[1]
				// Strip a trailing parenthetical like "(RedactRunsOlderThan)".
				ref = strings.TrimSpace(ref)
				if seen[ref] {
					continue
				}
				seen[ref] = true

				// A reference into a sibling repository names it, because that
				// repository is not in this checkout and existence cannot be
				// checked from here. Naming it is the point: a reader following
				// "lib/sanitizeHtml.ts" inside this repo finds nothing and
				// concludes the document is wrong, when the file is simply
				// somewhere else. This guard found three of those.
				if strings.Contains(ref, ": ") || strings.HasPrefix(ref, "onecamp-fe") {
					continue
				}

				// Only check things that look like real paths, not prose that
				// happens to contain a slash.
				isDir := strings.HasSuffix(ref, "/")
				if !isDir && !hasSourceExt(ref) {
					continue
				}
				checked++

				full := filepath.Join(repoRoot, ref)
				if _, err := os.Stat(full); err != nil {
					t.Errorf("%s cites %q and it does not exist; a reference a reader cannot follow "+
						"undermines every other row in the table", doc, ref)
				}
			}
			if checked == 0 {
				t.Fatalf("%s: the extractor matched nothing checkable, so this guard is asleep", doc)
			}
			t.Logf("%s: %d referenced paths verified", doc, checked)
		})
	}

	if found == 0 {
		t.Fatal("no compliance documents were found at all, so this guard checked nothing")
	}
}

func hasSourceExt(p string) bool {
	for _, ext := range []string{".go", ".sql", ".md", ".ts", ".tsx", ".yml", ".yaml"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}
