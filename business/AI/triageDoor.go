package business

import (
	"context"

	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// Seams for tests: whether a person is an AI teammate's own principal, and
// triage taking in a task just made.
var (
	isAgentPrincipal = userDomain.IsAgentPrincipal
	arriveInTriage   = triageBusiness.Arrive
)

// cameFromOutside is the triage door a task made through create_task came
// in by (linear-21 H1): an AI teammate making it as its own principal, or an
// API token or MCP acting as its member. The assistant in a member's session
// makes the member's own work, which never waits in triage.
func cameFromOutside(ctx context.Context, actor uuid.UUID) (triageBusiness.Source, bool) {
	switch {
	case isAgentPrincipal(ctx, actor):
		return triageBusiness.FromAgent, true
	case helpers.ActsThroughToken(ctx):
		return triageBusiness.FromAPI, true
	}
	return "", false
}
