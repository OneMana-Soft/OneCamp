package ai

// Executing a tool call that a person approved from an agent's proposal.
//
// A proposal is executed later, from the approval request, where nothing of the
// run that proposed it is on the context: not the agent, not the thread it was
// working in. Most tools do not care. One that starts background work which
// reports back where it was asked (code_pr) needs both, so the approval path
// says which agent the proposal came from — read from the stored proposal, never
// from the request — and the runner stores the thread on the proposal itself.

import (
	"context"
	"strings"
)

// ProposalSurfaceParam is the parameter on a stored proposal that carries the
// thread its run was working in, written by the runner when it proposes. It is
// honoured only while executing an approved agent proposal (see
// ApprovedAgentProposal), so the same parameter sent to the assistant's own
// execute endpoint does nothing.
const ProposalSurfaceParam = "_reply_surface"

type approvedProposalKeyT struct{}

var approvedProposalKey approvedProposalKeyT

// WithApprovedAgentProposal marks ctx as executing a proposal agentID made and
// a person approved.
func WithApprovedAgentProposal(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, approvedProposalKey, strings.TrimSpace(agentID))
}

// ApprovedAgentProposal reports the agent whose approved proposal ctx is
// executing, if it is executing one.
func ApprovedAgentProposal(ctx context.Context) (agentID string, ok bool) {
	agentID, _ = ctx.Value(approvedProposalKey).(string)
	return agentID, agentID != ""
}
