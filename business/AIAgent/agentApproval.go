package business

// An agent's proposal, approved later.
//
// business/AI runs an approved proposal AS the person approving it, from a
// request where nothing of the run that proposed it is on the context. This puts
// back what that run would decide, as things stand when the person approves:
//
//   - the agent must still exist and be switched on, since switching it off
//     stops its work, and work it proposed is its work;
//   - a tool from an MCP server runs only for an agent given it, so the agent
//     must still have it;
//   - a call made for someone other than the sponsor runs for that person again:
//     their reach is bound for the call, as in the run, and the call is refused
//     if they could no longer ask for it (requesterGate).

import (
	"context"
	"fmt"
	"slices"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func init() {
	aiBusiness.RegisterApprovedCallCheck(checkApprovedCall)
}

// agentForApproval loads the agent behind a proposal. A seam.
var agentForApproval = model.GetAgentByID

// checkApprovedCall decides one call agentID proposed, as a person approves it,
// and returns ctx bound to whoever the run was for (askedBy, or the sponsor alone
// when nil). A refusal is worded for the person approving.
//
// Fails closed and never panics: every lookup here is an authorization question,
// and the graph accessors behind the asker's reach dereference a client that is
// nil until the server has connected. A question that can't be answered is a
// refusal, never the sponsor's reach.
func checkApprovedCall(ctx context.Context, agentID, askedBy *uuid.UUID, call ai.ProposedAction) (bound context.Context, refusal string) {
	defer func() {
		if r := recover(); r != nil {
			bound, refusal = ctx, "This couldn't be checked for the person it was asked for, so it wasn't run. Ask the agent again."
		}
	}()
	const removed = "The agent that proposed this has been removed, so it wasn't run."
	if agentID == nil {
		return ctx, removed
	}
	agent, err := agentForApproval(ctx, *agentID)
	switch {
	case err != nil:
		return ctx, "This couldn't be checked against the agent that proposed it, so it wasn't run. Ask the agent again."
	case agent == nil:
		return ctx, removed
	case !agent.IsActive:
		return ctx, fmt.Sprintf("%s is switched off, so this wasn't run.", agent.Name)
	case ai.IsOutsideTool(call.ToolName) && !slices.Contains(agentEnabledTools(agent), call.ToolName):
		return ctx, fmt.Sprintf("%s can no longer use %s, so this wasn't run.", agent.Name, call.ToolName)
	}
	if askedBy == nil {
		return ai.WithoutRunRequester(ctx), ""
	}
	ctx = ai.WithRunRequester(ctx, askedBy.String(), agent.CreatedBy.String())
	if why := newRequesterGate(ctx).refusal(ctx, call); why != "" {
		return ctx, "This wasn't run: " + why + "."
	}
	return ctx, ""
}
