package business

import (
	"context"
	"testing"

	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Only work made from outside the app waits in triage (linear-21 H1): an AI
// teammate making it as its own principal, or an API token or MCP acting as
// its member. The assistant in a member's session makes the member's own
// work, which never waits.
func TestOnlyWorkFromOutsideTheAppWaitsInTriage(t *testing.T) {
	agent, member := uuid.New(), uuid.New()
	was := isAgentPrincipal
	t.Cleanup(func() { isAgentPrincipal = was })
	isAgentPrincipal = func(_ context.Context, id uuid.UUID) bool { return id == agent }
	session := context.Background()
	token := helpers.WithActsThroughToken(session)
	cases := []struct {
		name  string
		ctx   context.Context
		actor uuid.UUID
		src   triageBusiness.Source
		held  bool
	}{
		{"the assistant in a member's session", session, member, "", false},
		{"a token or MCP acting as the member", token, member, triageBusiness.FromAPI, true},
		{"an AI teammate as itself", session, agent, triageBusiness.FromAgent, true},
		{"an AI teammate a token asked for", token, agent, triageBusiness.FromAgent, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, held := cameFromOutside(c.ctx, c.actor)
			if src != c.src || held != c.held {
				t.Errorf("came by %q held %v, want %q %v", src, held, c.src, c.held)
			}
		})
	}
}
