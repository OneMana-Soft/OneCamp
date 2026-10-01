package aicoworker

// DM-able specialist agents in a 1:1 DM (Req 10.1). When a member starts a DM
// with an agent's bot principal (rather than the shared "OneCamp AI" coworker)
// and sends a message, this routes the message to THAT agent's runner and posts
// the agent's reply back into the DM authored as the agent's own badged
// identity. The shared coworker's DM path (handleChatMention) delegates here
// for any DM whose recipient is not the shared bot.
//
// This lives in the coworker package because it already owns the chat.created
// transport (typing + CreateChat-as-bot); the agent-side lookup + run is in
// business/AIAgent, keeping that package free of a chat dependency.

import (
	"context"
	"strings"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	agentbusiness "github.com/akashc777/OneCamp/business/AIAgent"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// handleAgentDM answers a 1:1 DM addressed to a DM-able agent's bot principal.
// No-op (returns silently) when the recipient is not a DM-able agent, so an
// ordinary human-to-human DM whose recipient simply is not the shared bot is
// never disturbed. AI-enabled is already checked by the caller.
func handleAgentDM(ctx context.Context, receiverID, senderID, groupID, text, messageID string) {
	if strings.TrimSpace(groupID) == "" || strings.TrimSpace(text) == "" || strings.TrimSpace(senderID) == "" {
		return
	}

	agent, agentBot, err := agentbusiness.AgentForDMPrincipal(ctx, receiverID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (agent DM): lookup failed: %v", err)
		return
	}
	if agent == nil || agentBot == nil || agentBot.UUID == "" {
		return // recipient is not a DM-able agent — leave the DM untouched
	}

	// Loop guard: never reply to the agent's own message. Posting the reply
	// re-emits chat.created with the agent principal as sender, which would
	// otherwise re-enter here.
	if senderID == agentBot.UUID || senderID == agentBot.DgraphUID {
		return
	}

	// Multimodal grounding: if the DM message (or its thread) carries images and
	// a vision model is configured, describe them so the agent can "see" the
	// screenshot a teammate shared. Best-effort; "" when unavailable.
	imageContext := chatImageContext(ctx, messageID)

	// Durable async opt-in: an agent flagged run_in_background answers the DM as
	// a DURABLE job (evolving in-thread status comment, survives restarts,
	// pauses/resumes on needs_human/budget) instead of one synchronous pass. On
	// any enqueue failure this returns false and we fall through to the
	// synchronous reply below, so a DM is never dropped.
	if agentbusiness.EnqueueDMRunIfBackground(ctx, agent, senderID, groupID, text, messageID, imageContext) {
		return
	}

	// Live "typing…" as the agent's own principal while the run executes, so a
	// slow/multi-step run reads as the teammate working rather than dead air.
	stopTyping := make(chan struct{})
	go runBotChatTyping(ctx, agentBot, groupID, stopTyping)
	reply := agentbusiness.RunAgentDMReply(ctx, agent, senderID, groupID, text, imageContext)
	close(stopTyping)

	reply = strings.TrimSpace(reply)
	if reply == "" {
		// Empty reply = a deliberate silence (transient throttle/breaker), so we
		// stay quiet rather than posting a generic error into the DM.
		return
	}

	// Post the reply into the DM authored as the agent's OWN principal (its
	// name, avatar, uuid + the is_bot "AI" badge), reusing the shared DM reply
	// path. The agent principal is a real users row, so building its UserInfo
	// and authoring a chat as it works exactly like the shared bot.
	postChatReply(ctx, agentBot, true, groupID, senderID, reply, nil)
}

// chatImageContext returns a vision-derived description block for the image
// attachments on a chat message (a DM or group message) and its thread
// comments, or "" when there is no vision model or no images. Best-effort;
// never blocks a run. Cheap-gated: when no vision model is configured it skips
// the Dgraph read entirely. The object keys come from a chat message the asker
// legitimately received (the triggering message), fetched server-side and
// re-validated as real images, mirroring the channel path.
func chatImageContext(ctx context.Context, messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return ""
	}
	if _, ok := svc.VisionClient(); !ok {
		return "" // no vision model → skip the read entirely
	}
	dgc, err := chatBusiness.GetDgraphChatByUUIDWithAllComments(ctx, messageID)
	if err != nil || dgc == nil {
		return ""
	}
	return agentbusiness.BuildImageContextFromRefs(ctx, agentbusiness.CollectImageRefs(dgc.MediaObj, dgc.Comments))
}

// handleChatCommentThread continues an agent's work when a human comments in a
// group-chat message's thread — the chat analog of the channel post.comment
// path. An agent runs if it is @mentioned in the comment; its reply lands as
// another comment on the SAME message (message_id), so answering the agent's
// needs_human question or re-@mentioning it in the thread picks the work back
// up. Loop-safe: an agent's own reply comment has IsBot=true and never
// dispatches chat.comment.created (see CreateChatComment). Inert when AI is off.
func handleChatCommentThread(ctx context.Context, data map[string]interface{}) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}
	messageID, _ := data["message_id"].(string)
	groupID, _ := data["group_id"].(string)
	senderID, _ := data["sender_id"].(string)
	text, _ := data["text"].(string)
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(groupID) == "" || strings.TrimSpace(text) == "" {
		return
	}
	// Durable resume: a human reply in this message's thread resumes any paused
	// (awaiting_input) durable run for it in place (group or DM), the chat analog
	// of answering a blocked task. Resumed agents are excluded from a fresh
	// dispatch so a follow-up never both resumes AND restarts the same agent.
	resumedDurable := agentbusiness.ResumeDurableChatRuns(ctx, messageID, senderID, text)

	// message_id is the PARENT chat message (the thread); the agent's reply is a
	// comment on it, so it lands in the same thread. Reuses the exact group
	// dispatch used for a top-level group @mention.
	dispatchGroupChatAgents(ctx, data, messageID, groupID, senderID, text, resumedDurable)
}

// dispatchGroupChatAgents runs every custom agent @mentioned in a group-chat
// message and posts each reply as an in-thread comment on the triggering
// message, authored as that agent. Mention detection + the run live in
// business/AIAgent; only the chat transport lives here. Each agent runs in its
// own goroutine (panic-guarded) so one slow/failed agent never blocks another
// or the shared coworker. Loop-safe: a bot reply is a chat comment, which emits
// no chat.created, and agent tool writes are workflow-tagged, so a group
// mention can never re-trigger an agent.
func dispatchGroupChatAgents(ctx context.Context, data map[string]interface{}, messageID, groupID, senderID, text string, resumedDurable map[uuid.UUID]bool) {
	mentionIDs := toStringSlice(data["mention_ids"])
	mentioned := agentbusiness.MentionedAgents(ctx, mentionIDs, text)
	for _, m := range mentioned {
		// Loop guard: never answer the agent's own message.
		if senderID == m.Bot.UUID || senderID == m.Bot.DgraphUID {
			continue
		}
		// A durable run already resumed for this agent on this message handles
		// the follow-up itself; don't also launch a fresh turn for it.
		if resumedDurable[m.Agent.Id] {
			continue
		}
		go runGroupChatAgent(context.WithoutCancel(ctx), m, messageID, groupID, senderID, text)
	}
}

// runGroupChatAgent executes one mentioned agent's group-chat turn under a
// detached, panic-guarded goroutine: it shows the agent's own "typing…"
// indicator while the run executes, then posts the reply as an in-thread
// comment. A silent (empty) reply — e.g. a transient throttle — posts nothing,
// avoiding a reply storm during an outage.
func runGroupChatAgent(ctx context.Context, m agentbusiness.MentionedAgent, messageID, groupID, senderID, text string) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("AI coworker (group agent): recovered panic (agent=%s): %v", m.Agent.Id, r)
		}
	}()

	// Multimodal grounding for the group message + its thread (best-effort, ""
	// when no vision model / no images), computed once and reused by both the
	// durable and synchronous paths.
	imageContext := chatImageContext(ctx, messageID)

	// Durable async opt-in: an agent flagged run_in_background answers the group
	// @mention as a DURABLE job (evolving in-thread status comment, survives
	// restarts, pauses/resumes on needs_human/budget) instead of one synchronous
	// pass. On any enqueue failure this returns false and we fall through to the
	// synchronous reply below, so a group @mention is never dropped.
	if agentbusiness.EnqueueGroupRunIfBackground(ctx, m.Agent, senderID, groupID, text, messageID, imageContext) {
		return
	}

	stopTyping := make(chan struct{})
	go runBotChatTyping(ctx, m.Bot, groupID, stopTyping)
	reply := agentbusiness.RunAgentGroupReply(ctx, m.Agent, senderID, groupID, text, imageContext)
	close(stopTyping)

	reply = strings.TrimSpace(reply)
	if reply == "" {
		return
	}
	postAgentGroupChatReply(ctx, m.Bot, messageID, groupID, reply)
}

// postAgentGroupChatReply posts a custom agent's group-chat answer as an
// in-thread comment on the triggering message (messageID), authored as the
// agent's own principal. Falls back to a new group message authored as the
// agent if the message id is absent or the comment write fails, so a reply is
// never lost.
func postAgentGroupChatReply(ctx context.Context, agentBot *userBusiness.BotIdentity, messageID, groupID, text string) {
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, agentBot.UUID)
	if err != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (group agent): cannot build agent identity %q: %v", agentBot.UUID, err)
		return
	}
	htmlText := toChatHTML(text)
	if strings.TrimSpace(messageID) != "" {
		if _, cerr := chatBusiness.PostChatCommentAsBot(ctx, botInfo, messageID, htmlText); cerr == nil {
			return
		} else {
			helpers.LogErrorWithContext(ctx, "AI coworker (group agent): in-thread reply failed on msg %s, falling back to group message: %v", messageID, cerr)
		}
	}
	if _, cerr := chatBusiness.CreateChatForGroup(ctx, &chatAdapter.ChatInfo{GrpUuid: groupID, TextHtml: htmlText}, botInfo, nil, nil); cerr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (group agent): failed to send group reply: %v", cerr)
	}
}
