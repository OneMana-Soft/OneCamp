package ai

import (
	"context"
	"testing"
)

func TestAnApprovedProposalNamesItsAgent(t *testing.T) {
	if _, ok := ApprovedAgentProposal(context.Background()); ok {
		t.Fatal("an ordinary context is not executing an approved proposal")
	}
	agentID, ok := ApprovedAgentProposal(WithApprovedAgentProposal(context.Background(), " agent-1 "))
	if !ok || agentID != "agent-1" {
		t.Fatalf("got (%q, %v)", agentID, ok)
	}
	if _, ok := ApprovedAgentProposal(WithApprovedAgentProposal(context.Background(), "")); ok {
		t.Error("an empty agent id must not read as an approved proposal")
	}
}
