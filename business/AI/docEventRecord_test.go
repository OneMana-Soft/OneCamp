package business

import (
	"context"
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/google/uuid"
)

// An agent's doc events are each one row of the AI record, under the person who
// asked it; a person's are not, and neither is one the docs audit records.
func TestAnAgentsDocEventIsOneRowOfTheAIRecord(t *testing.T) {
	asker, agent := uuid.New(), uuid.New()
	type row struct{ kind, action, category, summary string }
	var rows []row
	old := recordAgentRow
	defer func() { recordAgentRow = old }()
	recordAgentRow = func(_ context.Context, actor *uuid.UUID, _ string, kind, action, category, summary string, meta map[string]interface{}) error {
		if actor == nil || *actor != asker || meta["agent_uuid"] != agent.String() || meta["title"] != nil {
			t.Errorf("row %v %v", actor, meta)
		}
		rows = append(rows, row{kind, action, category, summary})
		return nil
	}
	byAgent := resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "doc", ResourceUUID: uuid.NewString(),
		Actor: resourceEventBusiness.Actor{UserUUID: asker.String(), AgentUUID: agent.String()}, Data: map[string]string{"proposal_id": "p1", "title": "secret"}}
	if !agentDocEvent(byAgent) {
		t.Fatal("an agent's proposal isn't wanted")
	}
	recordAgentDocEvent(context.Background(), byAgent)
	if len(rows) != 1 || rows[0] != (row{"agent", "agent.doc.proposal.created", "agent", "Proposed a change to a page"}) {
		t.Errorf("rows %+v", rows)
	}
	byPerson := byAgent
	byPerson.Actor.AgentUUID = ""
	deleted := byAgent
	deleted.Kind = resourceEventBusiness.DocDeleted
	if agentDocEvent(byPerson) || agentDocEvent(deleted) {
		t.Error("a person's event, or one the docs audit records, is wanted")
	}
}
