package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAgentBotEmailDeterministicAndScoped(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	e1 := AgentBotEmail(id)
	e2 := AgentBotEmail(id)
	if e1 != e2 {
		t.Fatalf("AgentBotEmail must be deterministic: %q != %q", e1, e2)
	}
	if !strings.HasSuffix(e1, "@bot.onecamp.local") {
		t.Fatalf("agent bot email must use the bot sentinel domain: %q", e1)
	}
	if !strings.Contains(e1, id.String()) {
		t.Fatalf("agent bot email must be scoped to the agent id: %q", e1)
	}
	// Distinct agents get distinct principals.
	other := AgentBotEmail(uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	if other == e1 {
		t.Fatal("distinct agents must map to distinct bot emails")
	}
}

func TestAgentBotUsernameUniquePerAgent(t *testing.T) {
	a := AgentBotUsername(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	b := AgentBotUsername(uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	if a == b {
		t.Fatal("agent bot usernames must be unique per agent (users.username is UNIQUE)")
	}
	if !strings.HasPrefix(a, "agent-bot-") {
		t.Fatalf("unexpected username format: %q", a)
	}
}
