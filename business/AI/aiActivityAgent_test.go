package business

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// The AI record said "an agent acted as you" and never which one, though the
// rows an agent writes name it. The person reading it could not tell which
// agent to pause.
func TestTheAIRecordNamesTheActingAgent(t *testing.T) {
	meta := `{"agent_id":"6f1c2b9e-0000-4000-8000-000000000001","agent_name":"Release Captain","run_id":"r-17","initiator":"person"}`
	items := auditsToActivity([]*auditModel.AuditEntry{{
		Action:    "mcp.tool_call.refused",
		ActorKind: "agent",
		Metadata:  &meta,
		CreatedAt: time.Now().UTC(),
	}})
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	it := items[0]
	if it.AgentName != "Release Captain" || it.AgentID != "6f1c2b9e-0000-4000-8000-000000000001" {
		t.Errorf("the agent is not named: id %q name %q", it.AgentID, it.AgentName)
	}
	if it.ActorKind != "agent" || it.RunID != "r-17" {
		t.Errorf("actor kind %q, run %q", it.ActorKind, it.RunID)
	}

	// A person's own row names no agent.
	human := auditsToActivity([]*auditModel.AuditEntry{{Action: "ai.settings.update", ActorKind: "human"}})
	if human[0].AgentName != "" || human[0].AgentID != "" {
		t.Errorf("a person's row named an agent: %+v", human[0])
	}

	// A run names its agent too.
	agentID := uuid.New()
	runs := runsToActivity([]*agentModel.AgentRunActivity{{AgentRun: agentModel.AgentRun{AgentId: agentID, Id: uuid.New()}, AgentName: " Release Captain "}})
	if runs[0].AgentName != "Release Captain" || runs[0].AgentID != agentID.String() {
		t.Errorf("a run's agent: %+v", runs[0])
	}
}

// The feed reads the keys the writers put the agent under. A writer that
// renamed one would leave the record nameless again without a test failing,
// so each is checked where it is written.
func TestAgentAuditWritersUseTheKeysTheFeedReads(t *testing.T) {
	for _, f := range []string{"../AIAgent/agentAudit.go", "../MCPServer/audit.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, key := range []string{metaAgentID, metaAgentName} {
			if !strings.Contains(string(src), `"`+key+`"`) {
				t.Errorf("%s no longer writes %q, which the AI record reads to name the agent", f, key)
			}
		}
	}
}
