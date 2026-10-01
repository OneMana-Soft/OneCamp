package ai

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Prompt assembly must use the REPORTING truncators, so shortening is never silent.
//
// WHY THIS IS ENFORCED. Trimming context or history turns a request that would not fit
// into one that does, which is the right trade and the reason the budget exists. The
// failure mode is not the trim, it is the silence: someone who cannot see that half a
// thread was dropped concludes the assistant is unreliable, which is indistinguishable
// from it being broken and much harder to diagnose. The reporting variants cost one
// extra argument, and forgetting them leaves no trace anywhere.
//
// The plain TruncateToTokenBudget / TrimHistoryToBudget remain correct for anything that
// is not a prompt shown to a person — a summariser's own input, a code-agent's diff
// context, generated release notes. Those declare it locally with a marker, the same
// convention the overflow-rescue check uses, so the reason lives at the call site.
const trimVisibilityExemptMarker = "trim-visibility-exempt:"

var silentTruncate = regexp.MustCompile(`\b(?:ai\.)?(?:limits\.)?(?:TruncateToTokenBudget|TrimHistoryToBudget)\(`)

func TestPromptAssemblyReportsWhatItTrimmed(t *testing.T) {
	roots := []string{"../../business", "../../controllers"}

	scanned, exempted := 0, 0
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this ratchet would silently pass: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			scanned++
			lines := strings.Split(string(raw), "\n")
			for i, line := range lines {
				code := line
				if c := strings.Index(code, "//"); c >= 0 {
					code = code[:c]
				}
				m := silentTruncate.FindString(code)
				if m == "" {
					continue
				}
				if markerNear(lines, i, trimVisibilityExemptMarker) {
					exempted++
					continue
				}
				t.Errorf("%s:%d uses %s, which trims without telling anyone.\n"+
					"  Use limits.TruncateForPrompt(ctx, ...) or limits.TrimHistoryForPrompt(ctx, ...) so the\n"+
					"  surface can report that the answer was built from less than the whole input. Silent\n"+
					"  shortening looks identical to an unreliable assistant.\n"+
					"  If this text is not a prompt shown to a person, add a comment containing %q\n"+
					"  and the reason.", path, i+1, m, trimVisibilityExemptMarker)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if scanned == 0 {
		t.Fatal("no Go files were scanned — this ratchet silently stopped enforcing anything")
	}
	if exempted == 0 {
		t.Error("no exemptions found — the non-prompt truncations should still carry their markers")
	}
}

// markerNear reports whether line i, or the contiguous comment block immediately above
// it, carries marker.
func markerNear(lines []string, i int, marker string) bool {
	if strings.Contains(lines[i], marker) {
		return true
	}
	for j := i - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(trimmed, "//") {
			return false
		}
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	return false
}
