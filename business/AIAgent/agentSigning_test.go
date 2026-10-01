package business

import (
	"testing"
	"time"

	actionLog "github.com/akashc777/OneCamp/models/postgres/AgentActionLog"
	"github.com/google/uuid"
)

func signedIntent(agent uuid.UUID) actionLog.Intent {
	run, as := uuid.New(), uuid.New()
	in := actionLog.Intent{ID: uuid.New(), RunID: &run, AgentID: agent, RunAsUserID: &as, ToolName: "append_to_doc", ParamsDigest: "abc123"}
	signIntent(&in, time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("IST", 19800)))
	return in
}

func TestSignedIntentsVerifyAndTamperingIsCaught(t *testing.T) {
	agent := uuid.New()
	good := signedIntent(agent)
	if good.IntentAt.Nanosecond()%1000 != 0 || good.IntentAt.Location() != time.UTC {
		t.Fatalf("intent time must be UTC microseconds, got %v", good.IntentAt)
	}

	altered := signedIntent(agent)
	altered.ToolName = "delete_channel"
	otherAgent := signedIntent(uuid.New()) // listed under agent, signed by another
	otherAgent.AgentID = agent
	unsigned := actionLog.Intent{ID: uuid.New(), AgentID: agent, ToolName: "x", ParamsDigest: "y", IntentAt: time.Now()}
	later := signedIntent(agent)
	later.IntentAt = later.IntentAt.Add(time.Second)

	r := verifyIntents(agent, []actionLog.Intent{good, altered, otherAgent, unsigned, later})
	if r.Checked != 5 || r.Valid != 1 || r.Unsigned != 1 || r.Invalid != 3 || len(r.Examples) != 3 {
		t.Fatalf("report %+v", r)
	}
	if r.PublicKey == "" {
		t.Fatal("the report carries the public key for checking elsewhere")
	}
}

func TestAgentKeysAreStableAndDistinct(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if string(AgentPublicKey(a)) != string(AgentPublicKey(a)) {
		t.Fatal("an agent's key is derived, so it is the same every time")
	}
	if string(AgentPublicKey(a)) == string(AgentPublicKey(b)) {
		t.Fatal("two agents never share a key")
	}
}
