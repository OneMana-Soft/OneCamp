package domain

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// The AI-free assertions have to run BEFORE the edition is faked, because
// RegisterFeature deliberately has no inverse: a feature is present when its
// package is linked, and a test cannot unlink one. So this is a single ordered
// function rather than several independent ones, and the order is the setup.
func TestClassifyBotAndBotNameFollowTheEdition(t *testing.T) {
	agentID := uuid.New()

	t.Run("without the AI edition linked", func(t *testing.T) {
		if helpers.FeatureRegistered(helpers.FeatureNameAI) {
			t.Fatal("something linked the AI package into this test binary; the AI-free half of this test cannot run")
		}

		// The bug this guards: a customer who bought the edition with no AI in
		// it got a bot called "OneCamp AI" posting their workflow messages.
		if name := SystemBotDisplayName(); strings.Contains(strings.ToLower(name), "ai") {
			t.Errorf("AI-free bot display name mentions AI: %q", name)
		}
		if user := SystemBotUsername(); strings.Contains(strings.ToLower(user), "ai") {
			t.Errorf("AI-free bot username mentions AI: %q", user)
		}
		if got := ClassifyBot(SystemBotEmail); got != BotKindAutomation {
			t.Errorf("system bot on an AI-free build = %q, want %q", got, BotKindAutomation)
		}
	})

	t.Run("classification that does not depend on the edition", func(t *testing.T) {
		cases := map[string]BotKind{
			AgentBotEmail(agentID):          BotKindAgent,
			SlackBridgeBotEmail:             BotKindBridge,
			CheckInBotEmail:                 BotKindCheckIn,
			"someone@example.com":           "",
			"":                              "",
			"unrecognised" + BotEmailDomain: BotKindUnknown,
			// Emails are compared case-insensitively and trimmed, because these
			// arrive from a graph store rather than from the constructor.
			"  " + strings.ToUpper(SystemBotEmail) + " ": BotKindAutomation,
		}
		for email, want := range cases {
			if got := ClassifyBot(email); got != want {
				t.Errorf("ClassifyBot(%q) = %q, want %q", email, got, want)
			}
		}
	})

	t.Run("with the AI edition linked", func(t *testing.T) {
		helpers.RegisterFeature(helpers.FeatureNameAI, func() bool { return true })

		if got := ClassifyBot(SystemBotEmail); got != BotKindAssistant {
			t.Errorf("system bot on an AI build = %q, want %q", got, BotKindAssistant)
		}
		if name := SystemBotDisplayName(); name != "OneCamp AI" {
			t.Errorf("AI bot display name = %q, want %q", name, "OneCamp AI")
		}
		// An agent is still an agent, not the assistant, on a build with AI.
		if got := ClassifyBot(AgentBotEmail(agentID)); got != BotKindAgent {
			t.Errorf("agent bot on an AI build = %q, want %q", got, BotKindAgent)
		}
	})
}

// The classifier keys on the email, so the constructors that create bot rows
// and the classifier that reads them must agree on the shape. They share the
// constants; this proves the sharing actually holds.
func TestBotEmailConstructorsMatchTheClassifier(t *testing.T) {
	if !strings.HasSuffix(SystemBotEmail, BotEmailDomain) {
		t.Errorf("SystemBotEmail %q is not under %q, so ClassifyBot would return no kind for it", SystemBotEmail, BotEmailDomain)
	}
	id := uuid.New()
	if want := AgentBotUsername(id) + BotEmailDomain; AgentBotEmail(id) != want {
		t.Errorf("AgentBotEmail = %q, want %q", AgentBotEmail(id), want)
	}
}
