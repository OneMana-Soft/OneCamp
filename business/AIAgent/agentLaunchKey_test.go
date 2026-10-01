package business

import (
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// agentLaunchSerialKey must serialize mentions PER (agent, channel) with a
// queue cap (so distinct mentions are answered in order, not dropped, and
// different channels run concurrently), while schedule/event runs serialize per
// agent with cap 1 (an overlapping run is dropped, not stacked).
func TestAgentLaunchSerialKey(t *testing.T) {
	agent := uuid.New()
	other := uuid.New()

	// Mention: per (agent, channel), queued (cap > 1).
	kA, capA := agentLaunchSerialKey(agent, model.TriggerMention, "chan-A")
	kB, capB := agentLaunchSerialKey(agent, model.TriggerMention, "chan-B")
	if kA == kB {
		t.Fatalf("same agent in different channels must use different keys (got %q for both)", kA)
	}
	if !strings.Contains(kA, agent.String()) || !strings.Contains(kA, "chan-A") {
		t.Fatalf("mention key should encode agent + channel, got %q", kA)
	}
	if capA <= 1 || capB <= 1 {
		t.Fatalf("mention cap should allow a queue (>1), got %d/%d", capA, capB)
	}

	// Two different agents in the same channel must not share a key.
	kOther, _ := agentLaunchSerialKey(other, model.TriggerMention, "chan-A")
	if kOther == kA {
		t.Fatalf("different agents must use different keys")
	}

	// Schedule/event: per agent, cap 1 (drop overlap, do not stack).
	for _, ts := range []string{model.TriggerSchedule, model.TriggerEvent} {
		k1, cap1 := agentLaunchSerialKey(agent, ts, "")
		k2, _ := agentLaunchSerialKey(agent, ts, "ignored-channel")
		if cap1 != 1 {
			t.Fatalf("%s cap should be 1 (drop overlap), got %d", ts, cap1)
		}
		if k1 != k2 {
			t.Fatalf("%s key must be per-agent regardless of channel, got %q vs %q", ts, k1, k2)
		}
		if !strings.Contains(k1, agent.String()) {
			t.Fatalf("%s key should encode the agent, got %q", ts, k1)
		}
	}

	// A mention key and a schedule key for the same agent must differ, so the
	// two intents never serialize against each other.
	mKey, _ := agentLaunchSerialKey(agent, model.TriggerMention, "chan-A")
	sKey, _ := agentLaunchSerialKey(agent, model.TriggerSchedule, "")
	if mKey == sKey {
		t.Fatalf("mention and schedule keys for one agent must differ")
	}
}
