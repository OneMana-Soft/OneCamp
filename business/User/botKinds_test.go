package business

import (
	"testing"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/google/uuid"
)

func TestKindsOf(t *testing.T) {
	agent, checkin, slack, odd := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	got := kindsOf(map[uuid.UUID]string{
		agent:   domain.AgentBotPrefix + uuid.NewString() + domain.BotEmailDomain,
		checkin: domain.CheckInBotEmail,
		slack:   domain.SlackBridgeBotEmail,
		odd:     "someone@example.com",
	})
	want := map[uuid.UUID]domain.BotKind{agent: domain.BotKindAgent, checkin: domain.BotKindCheckIn, slack: domain.BotKindBridge, odd: domain.BotKindUnknown}
	if len(got) != len(want) {
		t.Fatalf("got %d kinds, want %d: %v", len(got), len(want), got)
	}
	for id, kind := range want {
		if got[id.String()] != string(kind) {
			t.Errorf("%s: got %q, want %q", id, got[id.String()], kind)
		}
	}
}
