package ai

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every caller of a provider must go through ChatWithRescue / ChatStreamWithRescue.
//
// WHY THIS IS ENFORCED MECHANICALLY. A prompt the provider refuses as too long is a
// permanent failure: the identical retry fails identically, so a person is stuck with no
// way forward. The recovery is one function call, and forgetting it is invisible — the
// code compiles, reads fine, and works until someone pastes a long document or a channel
// gets busy. That is exactly what happened: the rescue shipped for business/AI and left
// eleven direct calls behind it in seven other packages, including the interactive
// assistant's own streaming endpoint. A convention in a comment would not have caught
// that; this does.
//
// The scope is every Go file under business/ and controllers/ rather than one package,
// because the last version of this check was package-scoped and that is precisely how it
// missed them.
//
// Exemptions are LOCAL: put `chat-rescue-exempt:` and a reason in a comment on or just
// above the call. Local rather than a list in this file so the reason sits where the
// decision is, and so moving the code moves its justification with it.
const chatRescueExemptMarker = "chat-rescue-exempt:"

// directProviderCall matches a provider invocation — llm.Chat(, svc.LLM.ChatStream( —
// while never matching our own wrappers, whose names continue past "Chat".
var directProviderCall = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_.]*\.Chat(?:Stream)?\(`)

func TestEveryProviderCallGoesThroughTheOverflowRescue(t *testing.T) {
	roots := []string{"../../business", "../../controllers"}

	scanned, offenders, exempted := 0, 0, 0
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
				m := directProviderCall.FindString(code)
				if m == "" || strings.Contains(m, "WithRescue") {
					continue
				}
				// An exemption may sit on the line or in the comment block above it.
				if exemptNear(lines, i) {
					exempted++
					continue
				}
				offenders++
				t.Errorf("%s:%d calls %s directly.\n"+
					"  Use ai.ChatWithRescue(ctx, llm, msgs, opts) — or ai.ChatStreamWithRescue — so a prompt\n"+
					"  the provider refuses as too long is shrunk and retried. Without it the request fails\n"+
					"  permanently: an identical retry overflows identically, and the person asking has no\n"+
					"  way to proceed.\n"+
					"  If this call must observe raw provider behaviour, add a comment containing\n"+
					"  %q and the reason.", path, i+1, m, chatRescueExemptMarker)
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
	// The two known exemptions (the admin self-test's two calls, and the agent loop's
	// main call) are load-bearing: if they vanish, either the code moved or someone
	// removed the marker, and both deserve a look.
	if exempted == 0 {
		t.Error("no exemptions found — the self-test and agent-loop calls should still carry their markers")
	}
	if offenders > 0 {
		t.Logf("scanned %d files, %d exempt call(s)", scanned, exempted)
	}
}

// exemptNear reports whether line i, or the contiguous comment block immediately above
// it, carries the exemption marker. The block form is what lets a reason be written at
// length instead of crammed onto the call.
func exemptNear(lines []string, i int) bool {
	if strings.Contains(lines[i], chatRescueExemptMarker) {
		return true
	}
	for j := i - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(trimmed, "//") {
			return false // the comment block ended without a marker
		}
		if strings.Contains(trimmed, chatRescueExemptMarker) {
			return true
		}
	}
	return false
}
