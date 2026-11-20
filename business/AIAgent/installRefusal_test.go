package business_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	marketplace "github.com/akashc777/OneCamp/business/Marketplace"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// CR-444 D1, the agent's half: what an agent's own create refuses is the
// person's to change, not ours to retry, so it comes back as a helpers.Refusal
// (400) in its words, and an install that wraps it the way Install does
// ("failed to install agent: %w") reads it as a 400 in only those words. Each
// refusal is made by the real CreateAgent on a path that reads no store: a
// model that isn't one, a blank name, a GitHub event (withdrawn) and an event
// not offered. The other half, that any kind's refusal keeps its status and
// words through the install route, is controllers/Marketplace's
// TestAnInstallItsKindRefusesKeepsItsWords, which may not import this package:
// the AI-free edition builds without it (helpers' aiEditionBoundary).
func TestWhatAnAgentsCreateRefusesAnInstallReadsInItsWords(t *testing.T) {
	for _, c := range []struct {
		in    agentBusiness.AgentInput
		words string
	}{
		{agentBusiness.AgentInput{Name: "Triage", ModelPref: "no-such-model"}, "invalid model selection"},
		{agentBusiness.AgentInput{Name: ""}, "name is required"},
		{agentBusiness.AgentInput{Name: "Reviewer", TriggerType: model.TriggerEvent, TriggerConfig: map[string]interface{}{"event": "github.pr.opened"}}, "GitHub events can't set off an agent"},
		{agentBusiness.AgentInput{Name: "Listener", TriggerType: model.TriggerEvent, TriggerConfig: map[string]interface{}{"event": "no.such.event"}}, "an agent can be set off only by one of the events offered"},
	} {
		_, made := agentBusiness.CreateAgent(context.Background(), c.in, uuid.New())
		var kind *helpers.Refusal
		if !errors.As(made, &kind) || kind.Status != http.StatusBadRequest || !strings.Contains(kind.Msg, c.words) {
			t.Errorf("%q: the create answered %#v, want a 400 helpers.Refusal in its words", c.words, made)
			continue
		}
		r, ok := marketplace.AsRefusal(fmt.Errorf("failed to install agent: %w", made))
		if !ok || r.Status != http.StatusBadRequest || r.Msg != kind.Msg {
			t.Errorf("%q: an install reads it as %#v (%v), want 400 with only %q", c.words, r, ok, kind.Msg)
		}
	}
}
