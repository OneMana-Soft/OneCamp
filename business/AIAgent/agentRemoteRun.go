package business

// How a run gets its remote brain, and how the remote's own work is recorded.

import (
	"encoding/json"
	"fmt"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// remoteProvider builds the provider for one run of a remote agent, in the
// protocol it speaks.
//
// Fails, rather than going out with no credential, when a secret is stored
// but cannot be read: the endpoint would refuse, or worse, accept, and either
// way the operator needs the real reason.
func remoteProvider(agent *model.AiAgent, runID string) (ai.RemoteBrain, error) {
	if agent.AGUIAuthUnreadable {
		return nil, fmt.Errorf("the remote agent's secret cannot be read (usually an AI_CONFIG_KEK change); re-enter it in the agent's settings")
	}
	if agent.IsA2A() {
		var card ai.A2ACard
		if err := json.Unmarshal(agent.RemoteCard, &card); err != nil || strings.TrimSpace(card.Endpoint) == "" {
			return nil, fmt.Errorf("this A2A agent's card has not been read; open the agent and save it again")
		}
		return ai.NewA2AProvider(card.Endpoint, agent.AGUIAuthHeader, agent.AGUIAuthSecret, runID, card.ProtocolVersion), nil
	}
	return ai.NewAGUIProvider(agent.AGUIEndpoint, agent.AGUIAuthHeader, agent.AGUIAuthSecret, runID), nil
}

// remoteLabel is how a run and the audit log name a remote brain: the protocol
// and the host, never the path or the secret.
func remoteLabel(agent *model.AiAgent) string {
	if agent.IsA2A() {
		return ai.RemoteLabel(ai.ProviderA2A, agent.AGUIEndpoint)
	}
	return ai.AGUILabel(agent.AGUIEndpoint)
}

// remoteWorkRecords drains what the provider saw the remote do for itself
// during the last call, as transcript records marked Remote. Nothing for a
// provider that is not a remote brain.
func remoteWorkRecords(llm ai.LLMProvider) []toolCallRecord {
	rr, ok := llm.(ai.RemoteToolReporter)
	if !ok {
		return nil
	}
	events := rr.TakeRemoteToolEvents()
	if len(events) == 0 {
		return nil
	}
	out := make([]toolCallRecord, 0, len(events))
	for _, ev := range events {
		name := strings.TrimSpace(ev.Name)
		if name == "" {
			name = "(unnamed)"
		}
		out = append(out, toolCallRecord{
			Tool:   name,
			Params: map[string]string{"arguments": ev.Arguments},
			Result: truncateObservation(ev.Result),
			Remote: true,
		})
	}
	return out
}
