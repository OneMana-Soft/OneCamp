package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsCallSessionKey(t *testing.T) {
	cases := map[string]bool{
		"call-RM_abc123":   true,
		"  call-RM_abc  ":  true,
		"EG_xyz789":        false, // a real egress id
		"":                 false,
		"recall-something": false, // must be a prefix, not a substring
	}
	for key, want := range cases {
		if got := IsCallSessionKey(key); got != want {
			t.Errorf("IsCallSessionKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// The key is built in the Python transcription agent and recognised here, so
// the prefix lives in two languages and nothing but this connects them.
//
// Getting it wrong is silent in the worst way. If the agent's prefix stops
// matching, every unrecorded call's transcript is filed under a key this side
// reads as a real recording: it appears in lists of playable recordings that
// have nothing to play, and a recap offers a "play recording" button that opens
// nothing. Nothing errors. So the two are pinned together here.
func TestCallSessionPrefixMatchesTheAgent(t *testing.T) {
	agent, err := os.ReadFile(filepath.Join("..", "livekit-agent", "agent.py"))
	if err != nil {
		t.Skipf("transcription agent not present in this checkout: %v", err)
	}
	want := `"` + callSessionPrefix + `"`
	if !strings.Contains(string(agent), want) {
		t.Errorf("livekit-agent/agent.py does not build keys with %s; the agent and this package have drifted", want)
	}
}
