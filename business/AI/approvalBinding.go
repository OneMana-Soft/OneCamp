package business

// What an approval is bound to.
//
// A proposal waits for a person, and approving it runs it later, from a request
// where nothing of the run that made it is on the context. The person approved
// three things as they were, and the approved call is held to all three:
//
//   - the call: its tool and its arguments, read from the stored proposal and
//     never from the approval request;
//   - where its tools send it: for a tool from an MCP server, that server and its
//     address (ai.CallBinding), fingerprinted when it was proposed. One that has
//     changed since is not sent;
//   - who it is for: the person the agent's run was for, when that was someone
//     other than its sponsor, recorded when it was proposed. The call runs for
//     them again, bounded by what they may do now (approvedCallCheck).
//
// And it runs at most once: ApprovePendingAction claims the row before anything
// runs, so a second approval of the same proposal finds it resolved.

import (
	"context"
	"errors"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// errUnknownAsker refuses a proposal from a run that someone asked for who could
// not be identified. Approved, it could only run with the sponsor's reach, which
// a run for someone else must never use.
var errUnknownAsker = errors.New("couldn't tell who asked for this, so it can't be proposed for them")

// destinationMovedMsg is why an approved call whose tools would send it somewhere
// other than where they did when it was proposed is not run.
const destinationMovedMsg = "A connector this uses has changed since it was proposed, so nothing was run. Ask again for a fresh proposal."

// bindingFor is what a proposal of calls to toolNames is bound to, decided where
// it is made: who the run on ctx is for, when that is not its sponsor, and where
// each tool sends its call.
func bindingFor(ctx context.Context, toolNames ...string) (pendingModels.Binding, error) {
	bind := pendingModels.Binding{ToolBinding: ai.CallBinding(toolNames...)}
	requester, _, forOther := ai.RunRequester(ctx)
	if !forOther {
		return bind, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(requester))
	if err != nil || id == uuid.Nil {
		return bind, errUnknownAsker
	}
	bind.AskedBy = &id
	return bind, nil
}

// approvedCallCheck decides, as a person approves it, one call an agent
// proposed, and returns ctx bound to whoever the agent's run was for. A refusal
// is worded for the person approving. Installed by business/AIAgent, which owns
// agents and the rule for whoever asked them; until it is, an agent's proposal
// does not run.
var approvedCallCheck func(ctx context.Context, agentID, askedBy *uuid.UUID, call ai.ProposedAction) (context.Context, string)

// RegisterApprovedCallCheck installs how a call an agent proposed is decided
// when a person approves it. Called once at init.
func RegisterApprovedCallCheck(fn func(ctx context.Context, agentID, askedBy *uuid.UUID, call ai.ProposedAction) (context.Context, string)) {
	approvedCallCheck = fn
}

// approvedCalls are the calls a proposal approves, read from the stored row: its
// one tool call, or each step of an agent's plan, in order.
func approvedCalls(action *pendingModels.PendingAction) ([]ai.ProposedAction, bool) {
	if action.ToolName != planSentinelTool {
		return []ai.ProposedAction{{ToolName: action.ToolName, Params: action.Params}}, true
	}
	steps, err := decodePlanSteps(action.Params["steps"])
	if err != nil || len(steps) == 0 {
		return nil, false
	}
	calls := make([]ai.ProposedAction, len(steps))
	for i, s := range steps {
		calls[i] = ai.ProposedAction{ToolName: s.ToolName, Params: s.Params}
	}
	return calls, true
}

// callToolNames are the tools calls use, in order.
func callToolNames(calls []ai.ProposedAction) []string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.ToolName
	}
	return names
}

// fromAgent reports whether a proposal was an agent's: it names the agent, or
// the person the agent's run was for. Either one holds its approval to the
// agent's rules, so a proposal whose agent has since been removed is refused
// rather than run as anyone else's.
func fromAgent(action *pendingModels.PendingAction) bool {
	return action.AgentID != nil || action.AskedBy != nil
}

// approvedExecutor resolves the tool an approved call names: OneCamp's own for
// any proposal, and one from an MCP server only for an agent's. An MCP tool is
// opted into twice, by the admin who registered its server and by the owner who
// gave it to an agent, so it runs only for an agent; anyone else's proposal of
// one has no executor, exactly as ExecuteAction has none for it.
func approvedExecutor(action *pendingModels.PendingAction, toolName string) (ai.ToolExecutor, bool) {
	if executor, ok := ai.Executors[toolName]; ok {
		return executor, true
	}
	if !fromAgent(action) {
		return nil, false
	}
	return ai.GetExecutor(toolName)
}

// executeApproved runs one call of an approved proposal AS the approver. An
// agent's proposal is decided first as its run would decide it now
// (approvedCallCheck): for the person it was for, and only while the agent may
// still use the tool.
func executeApproved(ctx context.Context, approverInfo *userModels.UserInfo, action *pendingModels.PendingAction, call ai.ProposedAction) (*adapter.ExecuteActionResponse, error) {
	if fromAgent(action) {
		if approvedCallCheck == nil {
			return nil, errors.New("an agent's proposal can't run before agents have started on this server")
		}
		bound, refusal := approvedCallCheck(ctx, action.AgentID, action.AskedBy, call)
		if refusal != "" {
			return &adapter.ExecuteActionResponse{Success: false, Message: refusal}, nil
		}
		ctx = bound
	}
	executor, ok := approvedExecutor(action, call.ToolName)
	return runConfirmedAction(ctx, approverInfo, call, executor, ok, nil)
}
