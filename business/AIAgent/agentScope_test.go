package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// TestAgentAllowedInChannel verifies the "invited or silent" mention gate:
// empty scope = respond anywhere; a non-empty channel scope restricts the
// agent to exactly the listed channels.
func TestAgentAllowedInChannel(t *testing.T) {
	cases := []struct {
		name      string
		scopeJSON string
		channelID string
		want      bool
	}{
		{"empty scope responds anywhere", "{}", "chan-1", true},
		{"empty channel_ids responds anywhere", `{"channel_ids":[]}`, "chan-1", true},
		{"in-scope channel responds", `{"channel_ids":["chan-1","chan-2"]}`, "chan-1", true},
		{"out-of-scope channel is silent", `{"channel_ids":["chan-2"]}`, "chan-1", false},
		{"malformed scope is treated as open", "not-json", "chan-1", true},
		{"whitespace-padded id still matches", `{"channel_ids":[" chan-1 "]}`, "chan-1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &model.AiAgent{Scope: c.scopeJSON}
			if got := agentAllowedInChannel(a, c.channelID); got != c.want {
				t.Fatalf("agentAllowedInChannel(%q, %q) = %v, want %v", c.scopeJSON, c.channelID, got, c.want)
			}
		})
	}
}
