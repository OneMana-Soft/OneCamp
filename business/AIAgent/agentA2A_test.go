package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func TestRemoteProtocolDefaultsToAGUIAndRefusesTheUnknown(t *testing.T) {
	for in, want := range map[string]string{"": "agui", "AGUI": "agui", " a2a ": "a2a"} {
		if got, err := validateRemoteProtocol(in); err != nil || got != want {
			t.Errorf("validateRemoteProtocol(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := validateRemoteProtocol("grpc"); err == nil {
		t.Error("an unknown protocol was accepted")
	}
}

// An A2A agent's card is read when it is saved, so a wrong address fails then,
// with the reason, and not at its first run.
func TestSavingAnA2AAgentReadsItsCard(t *testing.T) {
	old := fetchA2ACardFn
	t.Cleanup(func() { fetchA2ACardFn = old })
	fetchA2ACardFn = func(_ context.Context, given, header, secret string) (*ai.A2ACard, error) {
		if given == "https://bad.example" {
			return nil, errors.New("a2a: could not read the agent card: a2a: https://bad.example/.well-known/agent-card.json answered 404")
		}
		return &ai.A2ACard{Name: "Notes", Endpoint: "https://a.example/rpc", ProtocolVersion: "1.0"}, nil
	}
	raw, err := resolveRemoteCard(context.Background(), model.RemoteA2A, "https://a.example", "", "s")
	var card ai.A2ACard
	if err != nil || json.Unmarshal(raw, &card) != nil || card.Endpoint != "https://a.example/rpc" {
		t.Fatalf("card not stored: %s %v", raw, err)
	}
	if _, err := resolveRemoteCard(context.Background(), model.RemoteA2A, "https://bad.example", "", "s"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a bad address must fail with the reason: %v", err)
	}
	if raw, err := resolveRemoteCard(context.Background(), model.RemoteAGUI, "https://x", "", ""); raw != nil || err != nil {
		t.Fatal("AG-UI agents have no card")
	}
}

func TestAnA2AAgentRunsOnTheEndpointItsCardNamed(t *testing.T) {
	card, _ := json.Marshal(ai.A2ACard{Name: "Notes", Endpoint: "https://a.example/rpc", ProtocolVersion: "0.3.0"})
	a := &model.AiAgent{Id: uuid.New(), AGUIEndpoint: "https://a.example", RemoteProtocol: model.RemoteA2A, RemoteCard: card}
	p, err := remoteProvider(a, "run")
	if err != nil || p.Label() != "a2a:a.example" || remoteLabel(a) != "a2a:a.example" {
		t.Fatalf("got %v %v", p, err)
	}
	a.RemoteCard = nil
	if _, err := remoteProvider(a, "run"); err == nil || !strings.Contains(err.Error(), "save it again") {
		t.Fatalf("an agent without its card must say how to fix it: %v", err)
	}
}
