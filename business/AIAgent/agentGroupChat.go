package business

// Group-chat @mentions of a custom agent (the group analog of a channel
// mention). When an agent's name/handle or bot principal is @mentioned in a
// group chat, that agent runs with the message as input and its reply is posted
// back into the group as an in-thread comment on the triggering message —
// authored as the agent's own badged identity.
//
// Mention DETECTION + the agent RUN live here (business/AIAgent) so this
// package stays free of a chat-package dependency; the chat transport (typing +
// posting the reply comment) lives in the AICoworker package, which already
// owns the chat.created plumbing. This mirrors the DM split in agentDM.go.
//
// The runner executes AS the agent's owner with per-call permission re-checks
// (identical envelope to a channel mention / DM run), so a group mention never
// becomes an unscoped escalation path.

import (
	"context"
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// MentionedAgent pairs a matched mention-trigger agent with its resolved bot
// principal, so the chat responder can both loop-guard against the agent's own
// message and author the reply as the agent's own identity.
type MentionedAgent struct {
	Agent *model.AiAgent
	Bot   *userBusiness.BotIdentity
}

// MentionedAgents returns the active mention-trigger agents whose handle or bot
// principal is @mentioned by the given mention ids / message text, each paired
// with its resolved bot principal. It reuses the SAME matcher as the channel
// dispatcher (mentionMatchesAgent) so mention detection is identical across
// channels and group chats. An agent whose principal cannot be provisioned is
// skipped (it can't be authored as, so it can't reply). Safe before the trigger
// workers have started (falls back to a live query).
func MentionedAgents(ctx context.Context, mentionIDs []string, text string) []MentionedAgent {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	trigMu.RLock()
	started := trigStarted
	agents := mentionCache
	trigMu.RUnlock()
	if !started {
		live, err := model.ListActiveByTrigger(ctx, model.TriggerMention)
		if err == nil {
			agents = live
		}
	}

	lower := strings.ToLower(text)
	out := make([]MentionedAgent, 0, len(agents))
	for _, a := range agents {
		if !mentionMatchesAgent(ctx, a, mentionIDs, lower) {
			continue
		}
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil || bot.UUID == "" {
			continue // can't author as this agent — skip rather than fail
		}
		out = append(out, MentionedAgent{Agent: a, Bot: bot})
	}
	return out
}

// RunAgentGroupReply runs a mention-triggered agent for a single group-chat
// message and returns the reply text to post (empty string = stay silent, e.g.
// a transient throttle/breaker stop, so a group does not get a reply storm
// during an outage). Mirrors RunAgentDMReply: it reuses the agent runner (run
// AS the owner, permission-checked) and the shared mention-reply decision so a
// budget stop yields a brief explainable note rather than silence.
//
// Turns are serialized PER (agent, group) so a group's mentions of one agent
// are answered one at a time in order (a distinct mention is queued, never
// dropped, up to the flood cap); different groups and different agents run
// concurrently — the same shape as the channel mention queue.
func RunAgentGroupReply(ctx context.Context, a *model.AiAgent, senderID, groupID, text, imageContext string) string {
	key := "groupchat:" + a.Id.String() + ":" + groupID
	if !agentRunLock.Acquire(key, maxMentionTurnsPerChannel) {
		return "" // backlog full for this (agent, group) — shed the flood
	}
	defer agentRunLock.Release(key)

	prompt := groupChatPromptWithContext(ctx, a, senderID, groupID, text)
	if strings.TrimSpace(imageContext) != "" {
		prompt += imageContext
	}
	ctx = WithAgentRunScope(ctx, "", groupID)
	outcome := RunAgent(ctx, a, model.TriggerMention, prompt, false)
	reply, _ := agentMentionReply(outcome)
	return reply
}

// EnqueueGroupRunIfBackground enqueues a durable group-chat run for an agent
// opted into background execution, mirroring EnqueueDMRunIfBackground. Returns
// true when a durable job was enqueued (the caller must then skip the
// synchronous reply — the durable worker drives it with an evolving in-thread
// status comment), false otherwise (the caller falls back to the synchronous
// reply so a group @mention is never dropped). Carries the SAME enriched prompt
// (recent-group transcript continuity + optional image grounding) as the sync
// path.
// Same routing rule as a channel @mention (shouldRunMentionDurably), so a
// tool-capable teammate is stoppable and steerable in a group chat exactly as it
// is in a channel.
func EnqueueGroupRunIfBackground(ctx context.Context, a *model.AiAgent, senderID, groupID, text, messageID, imageContext string) bool {
	if a == nil || !shouldRunMentionDurably(a.RunInBackground, agentHasTools(a), durableMentionsEnabled()) {
		return false
	}
	prompt := groupChatPromptWithContext(ctx, a, senderID, groupID, text)
	if strings.TrimSpace(imageContext) != "" {
		prompt += imageContext
	}
	return EnqueueDurableAgentRun(ctx, a, Surface{Kind: SurfaceGroupChat, GroupID: groupID, MessageID: messageID}, prompt, senderID)
}

// groupChatPromptWithContext enriches the group-chat run prompt with the
// conversation's recent transcript (multi-turn continuity), read AS THE ASKER
// so it respects exactly what that member can see in the group — never as the
// agent owner, who may not be a participant. On any miss (asker unresolved,
// empty transcript) it returns the base single-turn prompt unchanged, so a run
// is never blocked by missing context. The agent's tool actions still execute
// within its owner envelope; only the read-only context is asker-scoped, so
// there is no confused-deputy.
func groupChatPromptWithContext(ctx context.Context, a *model.AiAgent, senderID, groupID, text string) string {
	base := synthGroupChatPrompt(text)
	if strings.TrimSpace(senderID) == "" || strings.TrimSpace(groupID) == "" {
		return base
	}
	askerInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, senderID)
	if err != nil || askerInfo == nil {
		return base
	}
	transcript := aiBusiness.GetRecentChatTranscript(ctx, askerInfo, groupID, agentSteerMaxMessages)
	if strings.TrimSpace(transcript) == "" {
		return base
	}
	return "Recent messages in this group chat (oldest first), for context — you may have replied earlier in it:\n\"\"\"\n" +
		transcript + "\n\"\"\"\n\n" + base
}

// synthGroupChatPrompt builds the run input for a group-chat mention: the
// member's message plus an instruction to compose the reply as the agent's
// final answer (it is posted as the agent in the group, so the agent must not
// use a send-message tool to reply here).
func synthGroupChatPrompt(text string) string {
	var b strings.Builder
	b.WriteString("You were mentioned in a group chat.\n\nTheir message:\n\"\"\"\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\"\"\"\n\nUse your tools as needed to take any actions, then compose a concise, helpful reply. " +
		"Your reply will be posted to the group as you (a badged AI teammate), so do NOT use a send-message tool to reply here — just write the reply as your final answer.")
	return b.String()
}
