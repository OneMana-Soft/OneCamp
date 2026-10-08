package business

// Which kind each bot is, for the tag beside its name. The one is_bot flag
// covers the assistant, agents, the Slack bridge, the channel-guest relay, the
// Check-in bot and, on the AI-free edition, the automation bot, and every one
// of them was tagged "Agent" (a screen reader said "AI agent") wherever it
// posted, even on a build with no AI in it. Only the assistant and agents have
// an AI behind them; the client tags the rest as plain bots from this.

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/User"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// BotKinds is every bot principal's kind, by uuid.
func BotKinds(ctx context.Context) (map[string]string, error) {
	emails, err := userModels.BotEmails(ctx)
	if err != nil {
		return nil, err
	}
	return kindsOf(emails), nil
}

// kindsOf classifies each bot by its email. A row flagged is_bot with an email
// the classifier doesn't know is still a bot: it reads as a plain one, never
// as nothing.
func kindsOf(emails map[uuid.UUID]string) map[string]string {
	out := make(map[string]string, len(emails))
	for id, email := range emails {
		kind := domain.ClassifyBot(email)
		if kind == "" {
			kind = domain.BotKindUnknown
		}
		out[id.String()] = string(kind)
	}
	return out
}
