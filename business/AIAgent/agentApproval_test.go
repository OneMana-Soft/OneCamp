package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// What an agent's run would decide about a call, decided again as a person
// approves it: the agent still there and switched on, an MCP tool still given
// to it, and a call for someone else still one they could ask for.
func TestAnApprovedCallIsDecidedAsTheRunWouldDecideItNow(t *testing.T) {
	ai.SetDynamicTools([]ai.ToolDef{{Name: "tracker_create_issue"}}, map[string]ai.ToolExecutor{
		"tracker_create_issue": func(context.Context, ai.ProposedAction, string) (string, map[string]string, error) {
			return "", nil, nil
		},
	})
	old := agentForApproval
	t.Cleanup(func() { agentForApproval = old; ai.SetDynamicTools(nil, nil) })

	sponsor, asker := uuid.New(), uuid.New()
	agentID := uuid.New()
	agent := &model.AiAgent{Id: agentID, Name: "Tracker", CreatedBy: sponsor, IsActive: true,
		EnabledTools: `["tracker_create_issue"]`}
	load := func(a *model.AiAgent, err error) {
		agentForApproval = func(context.Context, uuid.UUID) (*model.AiAgent, error) { return a, err }
	}
	mcpCall := ai.ProposedAction{ToolName: "tracker_create_issue", Params: map[string]string{"title": "Fix it"}}
	ctx := context.Background()

	refusals := []struct {
		name    string
		agentID *uuid.UUID
		agent   *model.AiAgent
		err     error
		want    string
	}{
		{"no agent recorded", nil, agent, nil, "removed"},
		{"the agent removed", &agentID, nil, nil, "removed"},
		{"the agent unreadable", &agentID, nil, errors.New("timeout"), "couldn't be checked"},
		{"the agent switched off", &agentID, &model.AiAgent{Id: agentID, Name: "Tracker", CreatedBy: sponsor, EnabledTools: `["tracker_create_issue"]`}, nil, "switched off"},
		{"the tool taken from the agent", &agentID, &model.AiAgent{Id: agentID, Name: "Tracker", CreatedBy: sponsor, IsActive: true, EnabledTools: `[]`}, nil, "can no longer use"},
	}
	for _, c := range refusals {
		load(c.agent, c.err)
		if _, refusal := checkApprovedCall(ctx, c.agentID, nil, mcpCall); !strings.Contains(refusal, c.want) {
			t.Errorf("%s: refusal %q should say %q", c.name, refusal, c.want)
		}
	}

	load(agent, nil)
	bound, refusal := checkApprovedCall(ai.WithRunRequester(ctx, asker.String(), sponsor.String()), &agentID, nil, mcpCall)
	if refusal != "" {
		t.Fatalf("the agent's own tool, for its sponsor, runs; got %q", refusal)
	}
	if _, asked := ai.RunAskedBy(bound); asked {
		t.Error("a call proposed for the sponsor alone runs for the sponsor alone, whatever the approval's context says")
	}

	// For someone else: bound to them and their sponsor, and decided by what they
	// may reach. With no graph to ask, that is a refusal, never the sponsor's reach.
	bound, refusal = checkApprovedCall(ctx, &agentID, &asker, mcpCall)
	if refusal == "" {
		t.Fatal("a call for someone whose reach can't be confirmed must be refused")
	}
	if requester, s, forOther := ai.RunRequester(bound); !forOther || requester != asker.String() || s != sponsor.String() {
		t.Fatalf("the call must be bound to the person the run was for and its sponsor; got %q, %q, %v", requester, s, forOther)
	}
}
