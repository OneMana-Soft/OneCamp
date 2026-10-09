package business

// Ambient dispatch — an opted-in agent may reply to a non-mention channel
// message when it judges it useful. This is the spam/cost-sensitive path, so it
// is gated in layers: only ambient-opted agents (loaded into ambientCache),
// only in channels they're scoped to, only messages that pass the cheap
// candidacy pre-filter (agentAmbient.go), at most one reply per (agent,channel)
// per cooldown window, metered against the agent + channel budgets, and the
// agent self-selects (stays silent via the NOTHING_TO_REPORT sentinel unless it
// can add real value). Mirrors launchRoutine.

import (
	"context"
	"fmt"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// triggerSourceAmbient labels ambient runs in the run history (distinct from a
// mention so the transcript shows the agent spoke up on its own).
const triggerSourceAmbient = "ambient"

// dispatchAmbientAgents considers each ambient agent for an unprompted reply to
// a freshly-posted channel message. Every gate is cheap and short-circuits, so
// this stays light on the per-message hot path.
func dispatchAmbientAgents(ctx context.Context, agents []*model.AiAgent, data map[string]interface{}) {
	if len(agents) == 0 {
		return
	}
	text, _ := data["text"].(string)
	if strings.TrimSpace(text) == "" {
		return
	}
	channelID, _ := data["channel_id"].(string)
	if channelID == "" {
		return
	}
	postID, _ := data["post_id"].(string)
	authorID, _ := data["author_id"].(string)
	mentionIDs := mentionIDsFromEvent(data["mention_ids"])

	for _, a := range agents {
		if !ambientConsiders(ctx, a, channelID, authorID, text, mentionIDs) {
			continue
		}
		// Per-(agent, channel) cooldown: at most one unprompted reply per window.
		if !ambientCooldownAcquire(ctx, a.Id.String(), channelID) {
			continue
		}
		launchAmbient(ctx, a, channelID, postID, authorID, text)
	}
}

// ambientConsiders reports whether an ambient agent should consider replying
// to a channel message: every gate but the cooldown, cheapest first.
func ambientConsiders(ctx context.Context, a *model.AiAgent, channelID, authorID, text string, mentionIDs []string) bool {
	// If the agent was @mentioned, the mention path already handles it.
	if mentionMatchesAgent(ctx, a, mentionIDs, strings.ToLower(text)) {
		return false
	}
	// Only in channels the agent is allowed in (explicit scope, or open).
	if !agentAllowedInChannel(a, channelID) {
		return false
	}
	// Never react to the agent's own message (defensive; the loop guard
	// already suppresses agent/automation posts from emitting events).
	if a.BotUserId != nil && authorID != "" && a.BotUserId.String() == authorID {
		return false
	}
	// Cheap candidacy pre-filter: questions / topic-keyword messages only.
	if !ambientCandidate(text, parseAmbientKeywords(a.AmbientKeywords)) {
		return false
	}
	// Only where the person it works for can read: it replies as them, and
	// its private note quotes the message to them. An agent with no channel
	// scope otherwise considered every channel, private ones included.
	ok, err := reach.readsChannel(ctx, a.CreatedBy.String(), channelID)
	return err == nil && ok
}

// ambientCooldownAcquire returns true only for the first attempt in the current
// window (INCR == 1). Fail-safe: a Redis outage yields 0 (not 1) → false, so an
// ambient agent stays quiet when it can't be rate-limited (never spams).
func ambientCooldownAcquire(ctx context.Context, agentID, channelID string) bool {
	count, err := redisStore.IncrByWithTTL(ctx, registry.AIAmbientCooldown, []string{agentID, channelID}, 1)
	if err != nil {
		return false
	}
	return count == 1
}

// launchAmbient runs one ambient consideration in its own goroutine and posts a
// concise in-thread reply on the triggering message ONLY when the agent judged
// it worthwhile (non-sentinel result). Serialized per (agent, channel) so
// overlapping messages don't stack.
//
// The run is asked for by the message's author. Nobody addressed the agent, but
// their words are what it acts on, and it replies in their thread: run as an
// unasked run, it had its sponsor's whole reach, so anyone posting where an
// agent listens could have that reach put to work for them. It now reaches only
// what the author and the sponsor both can; an author that cannot be identified
// refuses everything rather than borrow the sponsor's reach.
func launchAmbient(ctx context.Context, a *model.AiAgent, channelID, postID, authorID, triggerText string) {
	runCtx := WithAgentAskerWords(askedBy(context.WithoutCancel(ctx), authorID), triggerText)
	serialKey := fmt.Sprintf("ambient:%s:%s", a.Id, channelID)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.MessageLogs.ErrorLog.Printf("agentAmbient: recovered panic (agent=%s): %v", a.Id, rec)
			}
		}()
		if !agentRunLock.Acquire(serialKey, 1) {
			return
		}
		defer agentRunLock.Release(serialKey)

		var agentBot *userBusiness.BotIdentity
		if b, berr := userBusiness.EnsureAgentBot(runCtx, a.Id, a.Name, deref(a.AvatarKey)); berr == nil {
			agentBot = b
			if a.BotUserId == nil || *a.BotUserId != b.UserID {
				_ = model.SetAgentBotUser(runCtx, a.Id, b.UserID)
			}
		}
		if chUUID, perr := uuid.Parse(channelID); perr == nil {
			if cap, cerr := channelBusiness.GetChannelAITokenCap(runCtx, chUUID); cerr == nil {
				runCtx = ai.WithChannelBudget(runCtx, channelID, cap)
			}
		}
		runCtx = WithAgentRunScope(runCtx, channelID, "")

		// What the workspace already says about this in its public channels:
		// the cross-channel view a colleague has. Public only, because the same
		// run may reply in this channel, and only its instructions would keep a
		// private channel's text out of the reply.
		var related []string
		if hits, err := relatedSearch(runCtx, a.CreatedBy, triggerText); err == nil {
			related = relatedElsewhere(hits, channelID, relatedElsewhereMax, publicChannelFor(runCtx, a.CreatedBy.String()))
		}
		outcome := RunAgent(runCtx, a, triggerSourceAmbient, ambientRunPrompt(triggerText, related), false)

		// Reply in the thread, tell the sponsor privately, or stay silent
		// (see agentAmbientPrivate.go).
		deliverAmbientOutcome(runCtx, a, agentBot, channelID, postID, triggerText, outcome)
	}()
}

// ambientRunPrompt frames an unprompted consideration: add value or stay silent.
func ambientRunPrompt(triggerText string, relatedElsewhere []string) string {
	related := ""
	if len(relatedElsewhere) > 0 {
		related = "Related discussions in other channels (these may only be mentioned in a " + ambientPrivatePrefix + " note, never in this channel):\n- " +
			strings.Join(relatedElsewhere, "\n- ") + "\n" +
			"If this message and one of those are working on the same thing, or one already decided what this one is asking, that is worth a private note.\n\n"
	}
	return "A new message was posted in a channel you help with:\n\"\"\"\n" + triggerText + "\n\"\"\"\n\n" + related +
		"Reply ONLY if you can add clear, specific value right now — answer a question, correct a mistake, or surface a directly relevant fact (use your tools if needed). Keep it to a sentence or two. " +
		"If it is worth your sponsor knowing but should not be said here (it draws on another channel or a private conversation, or needs their decision first), reply with " + ambientPrivatePrefix + " followed by a short note for them alone. " +
		"A reply in this channel must draw only on this channel. " +
		"If you cannot add real value, reply with exactly " + scheduledNothingSentinel + " and nothing else. Do not greet or announce yourself; do not reply just to be polite."
}
