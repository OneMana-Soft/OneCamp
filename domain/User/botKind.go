package domain

import (
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// Bot principals are keyed on a synthetic email under one reserved domain.
// Shared here rather than written into each format string so that the
// classifier below and the constructors that create the rows cannot drift
// apart: a change to either shape is a change in one place.
const (
	// BotEmailDomain is the reserved domain every bot principal's email sits
	// under. Not a real mail domain and never delivered to.
	BotEmailDomain = "@bot.onecamp.local"
	// AgentBotPrefix begins the username and email of one agent's principal.
	AgentBotPrefix = "agent-bot-"
	// SlackBridgeBotUsername is the principal that carries messages from a
	// linked Slack channel, each badged with its Slack sender's name.
	SlackBridgeBotUsername = "slack-bridge"
	// SlackBridgeBotEmail keys that principal's row.
	SlackBridgeBotEmail = SlackBridgeBotUsername + BotEmailDomain
	// ChannelGuestBotUsername is the principal that carries messages from
	// people outside the workspace invited to a channel by a guest link, each
	// badged with the guest's name. Guests themselves never become users.
	ChannelGuestBotUsername = "channel-guest"
	// ChannelGuestBotEmail keys that principal's row.
	ChannelGuestBotEmail = ChannelGuestBotUsername + BotEmailDomain
	// CheckInBotUsername is the principal that asks channels' automatic
	// check-ins on their schedule.
	CheckInBotUsername = "checkin"
	// CheckInBotEmail keys that principal's row.
	CheckInBotEmail = CheckInBotUsername + BotEmailDomain
)

// BotKind names what a bot principal actually IS.
//
// WHY THIS EXISTS. is_bot is one boolean covering principals that do
// completely different jobs, and the frontend was treating all of them as the
// workspace assistant: a user-created agent's bot was titled "Assistant",
// offered a "Chat with AI" button and described as posting meeting recaps and
// running agents, none of which it does. On the AI-free edition the same copy
// appeared for a principal on a build with no AI in it at all.
//
// Classified from the email because that is the only stable marker. Both
// creation sites key their row on it, display names are reconciled on every
// boot and can be renamed, and nothing else on a bot row says which kind it is.
type BotKind string

const (
	// BotKindAssistant is the built-in workspace assistant: the principal that
	// posts recaps, answers @mentions and runs agents.
	BotKindAssistant BotKind = "assistant"
	// BotKindAutomation is the SAME principal on the AI-free edition, where it
	// exists only to carry workflow messages and does nothing with AI.
	BotKindAutomation BotKind = "automation"
	// BotKindAgent is one configured agent's own principal, which speaks only
	// as that agent.
	BotKindAgent BotKind = "agent"
	// BotKindBridge relays people from another chat app (the Slack bridge).
	// It has no AI and runs nothing: each message is someone in Slack.
	BotKindBridge BotKind = "bridge"
	// BotKindGuest relays people outside the workspace invited to a channel.
	// It has no AI: each message is a guest's, named in the message.
	BotKindGuest BotKind = "guest"
	// BotKindCheckIn asks channels' check-ins on their schedule. It has no AI:
	// each question is one a channel's moderators set up.
	BotKindCheckIn BotKind = "checkin"
	// BotKindUnknown is a bot this build does not recognise. Returned rather
	// than guessing, so a caller shows neutral copy instead of the assistant's.
	BotKindUnknown BotKind = "bot"
)

// ClassifyBot returns what kind of bot principal an email belongs to.
//
// Returns "" for a non-bot, so a caller can use the result directly without
// first testing is_bot.
func ClassifyBot(email string) BotKind {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.HasSuffix(email, BotEmailDomain) {
		return ""
	}
	switch {
	case email == SystemBotEmail:
		// One principal, two jobs. On a build with no AI it is not an
		// assistant and must not be described as one.
		if helpers.FeatureRegistered(helpers.FeatureNameAI) {
			return BotKindAssistant
		}
		return BotKindAutomation
	case email == SlackBridgeBotEmail:
		return BotKindBridge
	case email == ChannelGuestBotEmail:
		return BotKindGuest
	case email == CheckInBotEmail:
		return BotKindCheckIn
	case strings.HasPrefix(email, AgentBotPrefix):
		return BotKindAgent
	default:
		return BotKindUnknown
	}
}
