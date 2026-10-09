package business

// DM-able specialist agents (Req 10.1 / 12): an agent flagged dm_able can be
// addressed as a 1:1 DM target. A message sent to that agent's bot principal
// routes to THIS agent's runner (replying as the agent's own badged identity),
// distinct from the shared "OneCamp AI" coworker which handles its own DM.
//
// The actual DM transport (typing + posting the reply into the DM as the agent
// principal) lives in the AICoworker package, which already owns the chat.created
// plumbing; this file provides the agent-side lookup and run so AIAgent stays
// free of a chat-package dependency. The runner executes AS the agent's owner
// with per-call permission re-checks (the same envelope as a mention run), and
// is bounded by what the person DMing it can reach too (agentRequester.go), so
// a DM-able agent never lends its owner's access to whoever messages it.

import (
	"context"
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// DMTarget is an AI principal a member can start a 1:1 DM with, shaped like the
// user picker entries the new-DM dialog renders (so the FE can merge it into the
// same list). Only DM-able agents are returned; the shared coworker is surfaced
// separately through the global user list.
type DMTarget struct {
	UserUUID   string `json:"user_uuid"`
	UserName   string `json:"user_name"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
	Email      string `json:"user_email_id,omitempty"`
	JobTitle   string `json:"user_job_title,omitempty"`
	IsBot      bool   `json:"is_bot"`
}

// ListDMableAgentPrincipals returns the active DM-able agents resolved to their
// bot principals, shaped as DM-picker entries. Powers the new-DM people picker's
// "AI teammates" so a member can start a 1:1 DM with a specialist agent. The
// principal is provisioned on first use; an agent whose principal cannot be
// resolved is skipped (it simply is not offered) rather than failing the list.
func ListDMableAgentPrincipals(ctx context.Context) ([]DMTarget, error) {
	agents, err := dmAbleAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DMTarget, 0, len(agents))
	for _, a := range agents {
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil || bot.UUID == "" {
			continue
		}
		jobTitle := "AI teammate"
		if a.Description != nil && strings.TrimSpace(*a.Description) != "" {
			jobTitle = strings.TrimSpace(*a.Description)
		}
		out = append(out, DMTarget{
			UserUUID:   bot.UUID,
			UserName:   a.Name,
			ProfileKey: bot.ProfileKey,
			JobTitle:   jobTitle,
			IsBot:      true,
		})
	}
	return out, nil
}

// dmAbleAgents returns the cached active DM-able agents, falling back to a live
// query before the trigger workers have started (e.g. very early startup).
func dmAbleAgents(ctx context.Context) ([]*model.AiAgent, error) {
	trigMu.RLock()
	started := trigStarted
	cached := dmAbleCache
	trigMu.RUnlock()
	if started {
		return cached, nil
	}
	return model.ListDMable(ctx)
}

// AgentForDMPrincipal returns the active, DM-able agent whose bot principal
// matches receiverID (a DM recipient's user uuid OR Dgraph uid), together with
// the resolved principal. Returns (nil, nil, nil) when no DM-able agent matches
// — the common case for an ordinary human-to-human DM, so the coworker's DM
// path is left untouched. Used by the chat responder to decide whether a DM is
// addressed to a specialist agent.
func AgentForDMPrincipal(ctx context.Context, receiverID string) (*model.AiAgent, *userBusiness.BotIdentity, error) {
	receiverID = strings.TrimSpace(receiverID)
	if receiverID == "" {
		return nil, nil, nil
	}
	agents, err := dmAbleAgents(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, a := range agents {
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil {
			continue
		}
		if receiverID == bot.UUID || receiverID == bot.DgraphUID {
			return a, bot, nil
		}
	}
	return nil, nil, nil
}

// dmConversationLock serializes AI turns per DM conversation (keyed by the
// chat grouping id). Turns in the SAME DM are answered one at a time, in order,
// so replies never overlap or arrive out of sequence; different DMs (and other
// agents) run concurrently. This mirrors how mature chat platforms process a
// bot's messages — a real user turn is never dropped just because the agent is
// busy elsewhere (e.g. a scheduled run or another member's DM).
var dmConversationLock helpers.KeyedLock

// maxDMTurnsPerConversation bounds how many turns may be in-flight-or-queued
// for one DM at once (1 running + the rest queued). Beyond this the agent sheds
// the extra turn — backpressure against a flood, not normal conversation.
const maxDMTurnsPerConversation = 4

// RunAgentDMReply runs a DM-able agent for a single 1:1 DM message and returns
// the reply text to post (empty string = stay silent, e.g. a transient
// throttle/breaker stop, so a DM does not produce a reply storm during an
// outage). Reuses the agent runner (run AS the owner, permission-checked) and
// the shared mention-reply decision so a budget stop yields a brief explainable
// note rather than silence.
//
// Turns are serialized PER CONVERSATION (not dropped per-agent): a second
// message in the same DM waits for the first to finish rather than being lost,
// while the same agent can still answer other DMs / run scheduled jobs in
// parallel. A per-conversation backlog cap sheds only a genuine flood.
//
// senderID + groupID also drive multi-turn continuity: the agent is fed the
// DM's recent transcript (including its own earlier replies), read AS THE ASKER
// (a participant who legitimately sees the DM) — never as the agent owner, who
// is usually not in the DM. The run is for the sender: its tools execute as the
// owner but reach only what the sender can reach as well.
func RunAgentDMReply(ctx context.Context, a *model.AiAgent, senderID, groupID, text, imageContext string) string {
	if !dmConversationLock.Acquire(groupID, maxDMTurnsPerConversation) {
		// Backlog full for this DM (the member is flooding faster than the
		// agent can answer); shed this turn rather than pile up runs.
		return ""
	}
	defer dmConversationLock.Release(groupID)

	ctx = WithAgentAskerWords(askedBy(ctx, senderID), text)
	prompt := dmPromptWithContext(ctx, senderID, groupID, text)
	if strings.TrimSpace(imageContext) != "" {
		prompt += imageContext
	}
	ctx = WithAgentRunScope(ctx, "", groupID)
	outcome := RunAgent(ctx, a, "dm", prompt, false)
	reply, _ := agentMentionReply(outcome)
	return reply
}

// EnqueueDMRunIfBackground enqueues a durable DM run for an agent opted into
// background execution (run_in_background), returning true when a durable job
// was enqueued — the caller must then NOT run the synchronous reply, because
// the durable worker drives the run to completion with an evolving in-thread
// status comment (survives restarts, pauses/resumes on needs_human/budget).
// Returns false when the agent is not opted in OR the enqueue could not be
// performed, so the caller falls back to the synchronous reply and a DM is
// never dropped. The durable job carries the SAME enriched prompt (recent-DM
// transcript continuity + optional image grounding) as the synchronous path,
// so the async run has identical context.
// Routing matches a channel @mention exactly (shouldRunMentionDurably): an agent
// with tools goes durable, a conversation-only agent stays synchronous, and an
// explicit per-agent opt-in always wins. One rule for every surface is the point —
// a teammate you can stop in a channel but not in a DM is worse than either
// choice made consistently.
func EnqueueDMRunIfBackground(ctx context.Context, a *model.AiAgent, senderID, groupID, text, messageID, imageContext string) bool {
	if a == nil || !shouldRunMentionDurably(a.RunInBackground, agentHasTools(a), durableMentionsEnabled()) {
		return false
	}
	prompt := dmPromptWithContext(ctx, senderID, groupID, text)
	if strings.TrimSpace(imageContext) != "" {
		prompt += imageContext
	}
	return EnqueueDurableAgentRun(WithAgentAskerWords(ctx, text), a, Surface{Kind: SurfaceDM, GroupID: groupID, MessageID: messageID}, prompt, senderID)
}

// dmPromptWithContext enriches the DM run prompt with the conversation's recent
// transcript (multi-turn continuity), read AS THE ASKER so it respects exactly
// what that member can see in their own DM. On any miss (asker unresolved, empty
// transcript) it returns the base single-turn prompt unchanged, so a run is
// never blocked by missing context.
func dmPromptWithContext(ctx context.Context, senderID, groupID, text string) string {
	base := synthDMPrompt(text)
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
	return "Recent messages in this DM (oldest first), for context — you may have replied earlier in it:\n\"\"\"\n" +
		transcript + "\n\"\"\"\n\n" + base
}

// synthDMPrompt builds the run input for a 1:1 DM message: the member's message
// plus an instruction to compose the reply as the agent's final answer (it is
// posted as the agent in the DM, so the agent must not use a send-message tool
// to reply here).
func synthDMPrompt(text string) string {
	var b strings.Builder
	b.WriteString("A teammate is messaging you directly in a 1:1 DM.\n\nTheir message:\n\"\"\"\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\"\"\"\n\nUse your tools as needed to take any actions, then compose a concise, helpful reply. " +
		"Your reply will be posted to the DM as you (a badged AI teammate), so do NOT use a send-message tool to reply here — just write the reply as your final answer.")
	return b.String()
}
