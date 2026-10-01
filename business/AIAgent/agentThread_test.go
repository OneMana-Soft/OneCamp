package business

import (
	"strings"
	"testing"
)

// TestSynthThreadPrompt verifies the thread-continuation prompt carries the
// prior conversation (so a resumed run is coherent), the latest message, and
// the reply-in-thread instruction. Pure + DB-free.
func TestSynthThreadPrompt(t *testing.T) {
	p := synthThreadPrompt(
		"engineering",
		"Akash",
		"the owner is akashc777",
		"Release Captain: I need the owner of the onecamp-fe repository.",
	)
	for _, want := range []string{
		"engineering",
		"thread so far",
		"I need the owner of the onecamp-fe repository.",
		"the owner is akashc777",
		"from Akash",
		"do NOT use a send-message tool",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("thread prompt missing %q\n---\n%s", want, p)
		}
	}
}
