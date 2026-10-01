package business

import (
	"strings"
	"testing"
)

// The assistant must not pretend it can hold standing work, and must not
// pretend the product cannot do it.
//
// Routines exist and are created conversationally, but only inside an agent
// run: the assistant panel has no surface to attach one to. Before this, a
// person asking for "every Monday" in the panel got a refusal or an invention,
// and never learned the feature was there. That is a shipped capability made
// invisible by the one place people ask for it.
func TestAssistantPointsAtWhereRoutinesLive(t *testing.T) {
	p := askAISystemPrompt

	if !strings.Contains(p, "Standing work") {
		t.Fatal("the prompt says nothing about recurring work, so the assistant will improvise")
	}
	for _, signpost := range []string{"agent", "Agents"} {
		if !strings.Contains(p, signpost) {
			t.Fatalf("the prompt does not name where routines live: missing %q", signpost)
		}
	}
	if !strings.Contains(p, "NEVER claim you have scheduled") {
		t.Fatal("without this the assistant will say it scheduled something it cannot schedule")
	}
}
