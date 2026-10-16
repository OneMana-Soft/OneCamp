package business

import (
	"context"
	"errors"
	"testing"

	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// A proposal records who the agent's run was for when that was someone other
// than its sponsor, from the run itself, so the approved call can be held to
// them. A run someone asked for who couldn't be identified proposes nothing.
func TestAProposalIsBoundToWhoTheRunWasFor(t *testing.T) {
	sponsor, asker := uuid.NewString(), uuid.NewString()
	ctx := context.Background()
	for _, c := range []struct {
		name string
		ctx  context.Context
		want string
		err  error
	}{
		{"a run nobody asked for", ctx, "", nil},
		{"a run that says nobody asked", ai.WithoutRunRequester(ctx), "", nil},
		{"a run its sponsor asked for", ai.WithRunRequester(ctx, sponsor, sponsor), "", nil},
		{"a run someone else asked for", ai.WithRunRequester(ctx, asker, sponsor), asker, nil},
		{"a run asked for by someone nobody could identify", ai.WithRunRequester(ctx, "", sponsor), "", errUnknownAsker},
	} {
		t.Run(c.name, func(t *testing.T) {
			bind, err := bindingFor(c.ctx, "create_task")
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			got := ""
			if bind.AskedBy != nil {
				got = bind.AskedBy.String()
			}
			if err == nil && got != c.want {
				t.Fatalf("AskedBy = %q, want %q", got, c.want)
			}
		})
	}
}

// fakeOutsideTool registers one MCP tool sending its calls to destination, and
// counts the calls that reach it.
func fakeOutsideTool(t *testing.T, name, destination string) *int {
	t.Helper()
	calls := new(int)
	ai.SetDynamicTools([]ai.ToolDef{{Name: name, Destination: destination}}, map[string]ai.ToolExecutor{
		name: func(context.Context, ai.ProposedAction, string) (string, map[string]string, error) {
			*calls++
			return "done", nil, nil
		},
	})
	t.Cleanup(func() { ai.SetDynamicTools(nil, nil) })
	return calls
}

// An MCP tool runs only for an agent given it: from an agent's proposal, and
// from no one else's, whoever approves it.
func TestOnlyAnAgentsProposalReachesAnMCPTool(t *testing.T) {
	fakeOutsideTool(t, "tracker_create_issue", "server-1\x00https://tracker.example/mcp")
	agentID, asker := uuid.New(), uuid.New()
	for _, c := range []struct {
		name   string
		action *pendingModels.PendingAction
		want   bool
	}{
		{"the assistant's", &pendingModels.PendingAction{}, false},
		{"an agent's", &pendingModels.PendingAction{AgentID: &agentID}, true},
		{"one made for someone by an agent since removed", &pendingModels.PendingAction{AskedBy: &asker}, true},
	} {
		if _, ok := approvedExecutor(c.action, "tracker_create_issue"); ok != c.want {
			t.Errorf("%s proposal: resolves the MCP tool = %v, want %v", c.name, ok, c.want)
		}
	}
}

// The approval is for the call as proposed: a tool whose server moved since is
// not sent anything, and neither is the rest of the proposal.
func TestAnApprovedCallIsHeldToWhereItsToolSentIt(t *testing.T) {
	calls := fakeOutsideTool(t, "tracker_create_issue", "server-1\x00https://tracker.example/mcp")
	agentID := uuid.New()
	action := &pendingModels.PendingAction{
		ToolName: "tracker_create_issue", Params: map[string]string{"title": "Fix the login page"},
		AgentID: &agentID, ToolBinding: ai.CallBinding("tracker_create_issue"),
	}
	fakeOutsideTool(t, "tracker_create_issue", "server-1\x00https://elsewhere.example/mcp")
	resp, err := approveExecute(context.Background(), &userModels.UserInfo{}, action)
	if err != nil || resp == nil || resp.Success || resp.Message != destinationMovedMsg {
		t.Fatalf("a call whose tool now sends it elsewhere must be refused with %q; got %+v, %v", destinationMovedMsg, resp, err)
	}
	if *calls != 0 {
		t.Fatalf("nothing may reach the tool; it was called %d time(s)", *calls)
	}
}

// An agent's proposal runs only as its agent's rules decide it now, and not at
// all while those rules can't be asked.
func TestAnAgentsProposalRunsOnlyWhatItsCheckAllows(t *testing.T) {
	calls := fakeOutsideTool(t, "tracker_create_issue", "server-1\x00https://tracker.example/mcp")
	old := approvedCallCheck
	t.Cleanup(func() { approvedCallCheck = old })
	agentID := uuid.New()
	action := &pendingModels.PendingAction{ToolName: "tracker_create_issue", AgentID: &agentID}
	call := ai.ProposedAction{ToolName: "tracker_create_issue"}

	approvedCallCheck = func(ctx context.Context, _, _ *uuid.UUID, _ ai.ProposedAction) (context.Context, string) {
		return ctx, "Tracker is switched off, so this wasn't run."
	}
	resp, err := executeApproved(context.Background(), &userModels.UserInfo{}, action, call)
	if err != nil || resp == nil || resp.Success || resp.Message != "Tracker is switched off, so this wasn't run." {
		t.Fatalf("the check's refusal must be the outcome; got %+v, %v", resp, err)
	}

	approvedCallCheck = nil
	if _, err := executeApproved(context.Background(), &userModels.UserInfo{}, action, call); err == nil {
		t.Fatal("with no check installed, an agent's proposal must not run")
	}
	if *calls != 0 {
		t.Fatalf("nothing may reach the tool; it was called %d time(s)", *calls)
	}
}
