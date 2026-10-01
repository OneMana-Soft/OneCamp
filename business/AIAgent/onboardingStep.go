package business

// "Add an AI teammate", on the setup checklist.
//
// An agent is the reason to choose this edition, and a new workspace had no
// prompt to make one: the checklist asked for a model provider and then went
// straight to the governance drill, a demonstration of an agent being refused,
// in a workspace that had no agent of its own. Registered from here, so the
// AI-free edition never lists it.

import (
	"context"

	onboarding "github.com/akashc777/OneCamp/business/Onboarding"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// AgentStepID names the step.
const AgentStepID = "agent"

// AgentStepWeight places it after connecting a model provider (10) and before
// the governance drill (20).
const AgentStepWeight = 15

var agentStep = onboarding.Step{
	ID:     AgentStepID,
	Title:  "Add an AI teammate",
	Detail: "Give it a job and the tools it may use. It works as its sponsor, and asks before it changes anything until you say otherwise.",
	Href:   "/app/settings/agents",
}

// anyAgentExists is a seam.
var anyAgentExists = model.AnyAgentExists

func init() {
	onboarding.Register(agentStep, AgentStepWeight, nil, agentStepDone)
}

// agentStepDone reports whether the workspace has an agent. A failed lookup
// counts as not done, so the step stays visible rather than vanishing.
func agentStepDone(ctx context.Context, _ userModels.UserInfo) bool {
	ok, err := anyAgentExists(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AIAgent onboarding: agent lookup failed: %v", err)
		return false
	}
	return ok
}
