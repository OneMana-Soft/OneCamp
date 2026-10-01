package business

import (
	"strings"
	"testing"
)

// synthDMPrompt must embed the member's message and instruct the agent to write
// the reply as its final answer (it is posted as the agent in the DM, so the
// agent must not use a send-message tool to reply).
func TestSynthDMPrompt(t *testing.T) {
	p := synthDMPrompt("  what's blocking the launch?  ")
	if !strings.Contains(p, "what's blocking the launch?") {
		t.Fatalf("prompt missing the trimmed message:\n%s", p)
	}
	if !strings.Contains(p, "1:1 DM") {
		t.Fatalf("prompt should frame it as a direct message:\n%s", p)
	}
	if !strings.Contains(p, "do NOT use a send-message tool") {
		t.Fatalf("prompt should tell the agent not to send-message itself:\n%s", p)
	}
}
