package business

import (
	"strings"
	"testing"
)

func TestAgentVerifyEnabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"false": false,
		"0":     false,
		"off":   false,
		"no":    false,
		"true":  true,
		"1":     true,
		"on":    true,
		"YES":   true,
		"True":  true,
	}
	for val, want := range cases {
		t.Setenv("AI_AGENT_VERIFY", val)
		if got := agentVerifyEnabled(); got != want {
			t.Errorf("AI_AGENT_VERIFY=%q: agentVerifyEnabled = %v, want %v", val, got, want)
		}
	}
}

func TestBuildVerifyPrompt(t *testing.T) {
	got := buildVerifyPrompt("  We shipped 3 commits today.  ")

	// The draft must be embedded (trimmed).
	if !strings.Contains(got, "We shipped 3 commits today.") {
		t.Errorf("prompt missing draft, got:\n%s", got)
	}
	if strings.Contains(got, "  We shipped") {
		t.Errorf("draft should be trimmed, got:\n%s", got)
	}
	// Must instruct keep-as-is-when-supported and no new claims / no tools.
	lower := strings.ToLower(got)
	for _, want := range []string{"exactly as-is", "corrected version", "not add new claims", "not call any tool"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("prompt missing directive %q, got:\n%s", want, got)
		}
	}
}
