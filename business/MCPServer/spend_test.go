package business

import (
	"context"
	"os"
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// An agent-bound credential must carry the agent's OWN daily cap onto the call, so
// the per-agent budget engages over MCP exactly as it does for an in-app agent run.
// Carrying the id but not the limit would look correct and enforce nothing, which is
// the bug this closes.
func TestSpendContextAppliesTheAgentsOwnCap(t *testing.T) {
	actor := ActorIdentity{
		Type:             ActorAgent,
		TokenID:          "tok-1",
		AgentID:          "agent-1",
		AgentName:        "Researcher",
		AgentDailyTokens: 50000,
		PrincipalUserID:  "user-1",
	}

	ctx := SpendContext(context.Background(), actor)

	if got := ai.AgentBudgetID(ctx); got != "agent-1" {
		t.Fatalf("agent dimension not tagged: got %q, want %q", got, "agent-1")
	}
	if got := ai.AgentBudgetLimit(ctx); got != 50000 {
		t.Fatalf("agent cap not carried: got %d, want 50000 — the dimension would meter but never refuse", got)
	}
}

// A 0 cap means "no per-agent limit", and must still tag the dimension so the agent's
// spend appears in the per-agent usage breakdown.
func TestSpendContextMetersAnUncappedAgent(t *testing.T) {
	ctx := SpendContext(context.Background(), ActorIdentity{
		Type:            ActorAgent,
		AgentID:         "agent-2",
		PrincipalUserID: "user-1",
	})

	if got := ai.AgentBudgetID(ctx); got != "agent-2" {
		t.Fatalf("an uncapped agent must still be metered: got %q", got)
	}
	if got := ai.AgentBudgetLimit(ctx); got != 0 {
		t.Fatalf("expected no per-agent cap, got %d", got)
	}
}

// An unbound integration credential has no agent identity to bill, so no agent
// dimension may appear. Inventing one would attribute a script's spend to an agent
// that did not act.
func TestSpendContextAddsNoAgentDimensionForAnAPIClient(t *testing.T) {
	ctx := SpendContext(context.Background(), ActorIdentity{
		Type:            ActorAPIClient,
		TokenID:         "tok-2",
		PrincipalUserID: "user-1",
	})

	if got := ai.AgentBudgetID(ctx); got != "" {
		t.Fatalf("api_client must not carry an agent dimension, got %q", got)
	}
}

// Layered, not substituted: binding a token to an agent must not stop the call being
// attributed to the accountable human. Otherwise binding a credential to an agent
// would LOOSEN the user tier, and the whole point is that it can only tighten.
func TestSpendContextKeepsTheHumanAttribution(t *testing.T) {
	ctx := SpendContext(context.Background(), ActorIdentity{
		Type:             ActorAgent,
		AgentID:          "agent-3",
		AgentDailyTokens: 10,
		PrincipalUserID:  "user-7",
	})

	if got := ai.ActorID(ctx); got != "user-7" {
		t.Fatalf("human attribution lost when the agent dimension was added: got %q, want %q",
			got, "user-7")
	}
	if got := ai.AgentBudgetID(ctx); got != "agent-3" {
		t.Fatalf("agent dimension lost: got %q", got)
	}
}

// A nil context must not panic — this runs on a request path.
func TestSpendContextToleratesANilContext(t *testing.T) {
	//nolint:staticcheck // deliberately passing a nil context to prove it is handled
	ctx := SpendContext(nil, ActorIdentity{Type: ActorAgent, AgentID: "agent-4"})
	if ctx == nil {
		t.Fatal("SpendContext returned a nil context")
	}
	if got := ai.AgentBudgetID(ctx); got != "agent-4" {
		t.Fatalf("agent dimension lost on a nil parent: got %q", got)
	}
}

// THE RATCHET. The governed controller must run every tool handler on a
// spend-attributed context. Passing the bare request ctx is exactly the regression
// this closes: it looks completely correct, the tool works, the audit row is right,
// and the per-agent cap silently does nothing.
func TestGovernedHandlerRunsOnASpendAttributedContext(t *testing.T) {
	raw, err := os.ReadFile("../../controllers/MCP/governed.go")
	if err != nil {
		t.Fatalf("read governed.go: %v", err)
	}
	src := string(raw)

	const call = "decision.Spec.Handler("
	idx := strings.Index(src, call)
	if idx < 0 {
		t.Fatal("could not find the governed tool handler invocation — has it moved? " +
			"This ratchet must be updated to point at the new one.")
	}

	// Every invocation of the handler, not just the first.
	for idx >= 0 {
		line := src[idx : idx+min(len(src)-idx, 120)]
		if end := strings.Index(line, "\n"); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, "SpendContext(") {
			t.Fatalf("a governed tool handler runs on a context with no spend attribution:\n  %s\n"+
				"  -> wrap it: mcpBusiness.SpendContext(ctx, decision.Actor). Without it the "+
				"per-agent daily token cap is ignored on this surface.", strings.TrimSpace(line))
		}
		next := strings.Index(src[idx+len(call):], call)
		if next < 0 {
			break
		}
		idx = idx + len(call) + next
	}
}
