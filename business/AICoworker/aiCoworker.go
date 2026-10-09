// Package aicoworker turns the shared automation bot into an @mentionable AI
// coworker. When a user @mentions the bot in a channel, the bot replies IN
// THAT CHANNEL with an answer grounded in the channel's own messages.
//
// Phase 2, increment 2 — VISIBLE channel coworker, kept safe by scope.
//
//   - Read-only: a mention has no inline confirmation surface, so the coworker
//     answers/summarizes but never performs write actions. Writes stay gated to
//     the interactive assistant where the user can confirm them.
//   - Privacy-safe public reply: the answer is grounded ONLY in the channel's
//     own recent messages (business.AnswerChannelQuestion) — content every
//     member of that channel can already see — so broadcasting it cannot leak
//     the asker's DMs, private channels, or memory. This is why it does NOT use
//     the full-visibility AskAI on the public path.
//
// Architecture mirrors the Workflow engine: one in-process event-bus listener
// on post.created. The only message-hot-path change is an additive mention_ids
// field on that event. Model-agnostic — goes through svc.LLM.Chat, so it works
// on Ollama, OpenAI, Anthropic, or a custom endpoint.
package aicoworker

import (
	"context"
	"errors"
	"html"
	"strings"
	"sync"
	"time"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	agentbusiness "github.com/akashc777/OneCamp/business/AIAgent"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	codeagent "github.com/akashc777/OneCamp/business/CodeAgent"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// botFallbackMessage is posted when generation fails for a reason other than
// throttling/breaker, so a user who explicitly @mentioned the bot is never met
// with silence.
const botFallbackMessage = "I couldn't put together an answer just now. Please mention me again in a moment."

// Token-cap messages. Unlike a transient rate-limit/breaker (where staying
// silent avoids a reply storm during an outage), an admin-set token budget is a
// deliberate, explainable state — so when a user explicitly addressed the bot
// we tell them plainly why it paused, and who can change it.
const (
	botBudgetWorkspaceMessage = "I've paused for now — this workspace has reached today's AI usage limit. It resets tomorrow, or an admin can raise the limit in AI settings."
	botBudgetUserMessage      = "I've paused for now — today's AI usage limit for your account has been reached. It resets tomorrow, or an admin can raise it in AI settings."
)

// coworkerGreetingReply is the one friendly response a social @mention gets.
// It asks what the person needs without running the full Q&A model, so a bare
// "hi" is cheap and never turns into a loop.
func coworkerGreetingReply() string {
	return "Hi there! How can I help you today?"
}

// typingRepublishInterval re-asserts the bot's "typing…" indicator so it does
// not expire on clients before a slow local model finishes generating.
const typingRepublishInterval = 4 * time.Second

var (
	startMu sync.Mutex
	started bool
)

// Start wires the @mention responder into the event bus. Idempotent; call once
// at startup after the DB and automation bot are ready. The double-start guard
// prevents duplicate replies if Start is ever called twice.
func Start(ctx context.Context) {
	startMu.Lock()
	defer startMu.Unlock()
	if started {
		return
	}
	started = true
	webhookBusiness.RegisterEventListener(handleEvent)
	// Supply the chat (group/DM) status poster to the durable agent worker so a
	// long-running chat mention shows evolving in-thread progress; AIAgent stays
	// chat-dependency-free (it only calls this injected factory).
	agentbusiness.RegisterChatStatusPosterFactory(newChatStatusPoster)
	helpers.MessageLogs.InfoLog.Println("AI coworker (mention responder) started")
}

// handleEvent is the event-bus listener. Channel posts (post.created) and
// DM/group chats (chat.created) are both handled: the bot answers an @mention
// in a channel or group, and answers every message in a 1:1 DM with it.
func handleEvent(ctx context.Context, eventType string, data map[string]interface{}) {
	switch eventType {
	case "post.created":
		handleChannelMention(ctx, data)
	case "chat.created":
		handleChatMention(ctx, data)
	case "chat.comment.created":
		handleChatCommentThread(ctx, data)
	case "github.issue.opened":
		handleIssueTriage(ctx, data)
	case "github.pr.opened":
		handlePRReview(ctx, data)
	case "github.pr.closed":
		handlePRClosedOutcome(ctx, data)
	case "github.pr.comment":
		handlePRComment(ctx, eventType, data)
	case "github.pr.review_submitted":
		handlePRComment(ctx, eventType, data)
	}
}

// handlePRClosedOutcome records the terminal outcome of an agent-opened PR into
// the code_pr_runs ledger (the learning loop's ground-truth merge signal),
// matched by pr_url. Best-effort + generic: a no-op for any PR no agent run
// opened (the common case), so it's a cheap probe on every closed PR. Merge vs
// merged_with_edits is not distinguished here — GitHub's webhook only tells us
// "merged"; detecting post-open human edits is a later refinement.
func handlePRClosedOutcome(ctx context.Context, data map[string]interface{}) {
	prURL, _ := data["pr_url"].(string)
	if strings.TrimSpace(prURL) == "" {
		return
	}
	merged, _ := data["merged"].(bool)
	outcome := string(codepr.OutcomeClosed)
	if merged {
		outcome = string(codepr.OutcomeMerged)
	}
	if n, err := aiModels.SetCodePROutcomeByPRURL(ctx, prURL, outcome); err != nil {
		helpers.LogErrorWithContext(ctx, "aicoworker: capture PR outcome failed for %s: %v", prURL, err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx, "aicoworker: recorded code-PR outcome %q for %s (%d run(s))", outcome, prURL, n)
	}
}

func handleChannelMention(ctx context.Context, data map[string]interface{}) {
	// Gate: AI must be enabled. svc.IsEnabled() covers config + a live model,
	// and works identically across Ollama / OpenAI / Anthropic / custom.
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}

	channelID, _ := data["channel_id"].(string)
	channelName, _ := data["channel_name"].(string)
	text, _ := data["text"].(string)
	// What was asked, for the same reason the channel paths record it: a
	// connector that is down should be mentioned only where it would have
	// mattered.
	ai.NoteQuestion(ctx, text)
	authorID, _ := data["author_id"].(string)
	postID, _ := data["post_id"].(string)
	if channelID == "" || strings.TrimSpace(text) == "" || authorID == "" {
		return
	}
	if strings.TrimSpace(channelName) == "" {
		channelName = "this channel"
	}

	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || (bot.UUID == "" && bot.DgraphUID == "") {
		return
	}

	// Never respond to the bot's own messages (defence-in-depth; BotPost does
	// not emit post.created, so this also cannot loop).
	if authorID == bot.UUID || authorID == bot.DgraphUID {
		return
	}

	// Only respond when the bot was explicitly @mentioned — inherently opt-in
	// per message. Match either the Dgraph node uid (native mentions) or the
	// user uuid (Slack-import encoding), so detection is robust to both forms.
	if !mentionsBot(data["mention_ids"], bot.DgraphUID, bot.UUID) {
		return
	}

	// Admin gate: the coworker can be disabled independently of AI chat. Read
	// the setting live (cheap singleton row) only after we know the bot was
	// actually mentioned, so the common non-mention path stays free of a DB
	// hit. Defaults ON, so an existing workspace keeps working until an admin
	// opts out.
	if settings, serr := aiModels.GetSettings(ctx); serr != nil || !settings.Enabled || !settings.CoworkerEnabled {
		if serr != nil {
			helpers.LogErrorWithContext(ctx, "AI coworker: cannot read settings: %v", serr)
		}
		return
	}

	chUUID, perr := uuid.Parse(channelID)
	if perr != nil {
		return
	}

	// Ensure the coworker is a real member of this channel before it replies
	// (Req 8): an explicit @mention is an invitation, so we make "the AI is in
	// this channel" a true, queryable fact (it then shows in the roster) rather
	// than a drive-by responder. Idempotent + best-effort: a failure here never
	// blocks the reply.
	ensureCoworkerChannelMembership(ctx, bot, chUUID)

	// Build the mentioning user's identity. It is used only as the permission
	// filter for the channel-scoped fetch, so the answer is restricted to a
	// channel the asker can actually access.
	userInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, authorID)
	if err != nil || userInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker: cannot resolve mentioning user %q: %v", authorID, err)
		return
	}

	question := cleanQuestion(text, bot.Name)

	// Etiquette guard: don't spend tokens answering a social greeting or a clear
	// stop request. A greeting gets one friendly reply; a dismissal is silent.
	switch agentbusiness.ClassifyMentionIntent(text, nil) {
	case agentbusiness.IntentGreeting:
		postReply(ctx, bot, chUUID, channelID, postID, coworkerGreetingReply())
		return
	case agentbusiness.IntentDismiss:
		return
	}

	// Per-channel daily AI cost control (Claude-Tag-style): meter this reply on
	// the channel's budget dimension and refuse once the channel's daily cap is
	// hit. Best-effort read; a 0 cap meters without capping. Applies to the
	// shared coworker's spend here; agents add the same dimension on their runs.
	if cap, cerr := channelBusiness.GetChannelAITokenCap(ctx, chUUID); cerr == nil {
		ctx = ai.WithChannelBudget(ctx, channelID, cap)
	}

	// Stream the answer live into a single bot message (Notion/Slack-class
	// "watch it type") instead of posting the whole reply at once. When the
	// triggering post id is known, the reply streams into an in-thread COMMENT
	// on that message (Slack-style threaded reply); otherwise it streams into a
	// top-level channel message. Falls back to a one-shot post if streaming
	// can't start.
	streamChannelReply(ctx, bot, userInfo, chUUID, channelID, channelName, postID, question)
}

// streamChannelReply generates the coworker's channel answer and streams it
// live into one bot message, then finalizes it (persisted). A separate "typing"
// indicator covers the gap before the first token; once content flows, the
// live-updating message is the indicator. Error handling mirrors the
// non-streamed path: silent on transient throttle/breaker (and any partial
// message is removed), an explainable note on a token-budget pause, a brief
// honest note otherwise. Falls back to streamlessChannelReply if the stream
// cannot be started.
func streamChannelReply(ctx context.Context, bot *userBusiness.BotIdentity, userInfo *userModels.UserInfo, chUUID uuid.UUID, channelID, channelName, postID, question string) {
	// Records any shortening of the prompt so the reply can say it happened. Idempotent,
	// so the streamless fallback below reuses this one rather than starting a second.
	ctx = ai.WithContextNoticeSink(ctx)
	ai.NoteQuestion(ctx, question)
	stream, serr := beginChannelReplyStream(ctx, bot, chUUID, postID)
	if serr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker: stream begin failed, falling back to one-shot: %v", serr)
		streamlessChannelReply(ctx, bot, userInfo, chUUID, channelID, channelName, postID, question)
		return
	}

	stopTyping := make(chan struct{})
	var stopOnce sync.Once
	stopTypingFn := func() { stopOnce.Do(func() { close(stopTyping) }) }
	go runBotTyping(ctx, bot, channelID, stopTyping)

	final, aerr := aiBusiness.AnswerChannelQuestionStream(ctx, userInfo, channelID, channelName, question, func(running string) {
		stopTypingFn() // first content: the live message itself is now the indicator
		stream.Push(ctx, running)
	})
	stopTypingFn()

	if aerr != nil {
		// Transient throttle/breaker: stay silent (no reply storm during an
		// outage) and discard any partial message already posted.
		if errors.Is(aerr, ai.ErrRateLimited) || errors.Is(aerr, ai.ErrCircuitOpen) {
			helpers.LogInfoWithContext(ctx, "AI coworker: skipped (throttled/breaker): %v", aerr)
			stream.Abort(ctx)
			return
		}
		// Token caps: the user addressed the bot, so finalize with the
		// explainable pause message (Finalize creates the post if none yet).
		if errors.Is(aerr, ai.ErrWorkspaceTokenBudgetExceeded) {
			finalizeStream(ctx, stream, botBudgetWorkspaceMessage)
			return
		}
		if errors.Is(aerr, ai.ErrUserTokenBudgetExceeded) {
			finalizeStream(ctx, stream, botBudgetUserMessage)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI coworker: generation failed: %v", aerr)
		finalizeStream(ctx, stream, botFallbackMessage)
		return
	}

	final = strings.TrimSpace(final)
	if final == "" {
		finalizeStream(ctx, stream, botFallbackMessage)
		return
	}
	// Applied on the SUCCESS path only. The fallback and budget messages above are
	// about a reply that did not happen, and a footnote explaining how the context was
	// shortened would be describing an answer that does not exist.
	finalizeStream(ctx, stream, ai.AppendNoticeFootnote(final, ai.TakeContextNotice(ctx)))
}

// finalizeStream commits the stream's final text, logging a finalize failure
// without surfacing it (the user already saw the streamed content). Works on
// either a top-level post stream or an in-thread comment stream.
func finalizeStream(ctx context.Context, stream botpost.ReplyStream, text string) {
	if _, err := stream.Finalize(ctx, text); err != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker: stream finalize failed: %v", err)
	}
}

// beginChannelReplyStream opens the right live-reply stream for a coworker
// channel answer: an in-thread COMMENT stream on the triggering post when its
// id is known (Slack-style threaded reply), or a top-level channel post stream
// otherwise. A bad/absent post id transparently falls back to the top-level
// stream so a reply is never lost.
func beginChannelReplyStream(ctx context.Context, bot *userBusiness.BotIdentity, chUUID uuid.UUID, postID string) (botpost.ReplyStream, error) {
	if strings.TrimSpace(postID) != "" {
		if postUUID, perr := uuid.Parse(postID); perr == nil {
			return botpost.BeginPostCommentStream(ctx, postUUID, bot, "")
		}
	}
	return botpost.BeginChannelStream(ctx, chUUID, bot, "")
}

// streamlessChannelReply is the original non-streamed reply path, kept as a
// fallback for the rare case the stream cannot be started.
func streamlessChannelReply(ctx context.Context, bot *userBusiness.BotIdentity, userInfo *userModels.UserInfo, chUUID uuid.UUID, channelID, channelName, postID, question string) {
	ctx = ai.WithContextNoticeSink(ctx)
	ai.NoteQuestion(ctx, question)
	stopTyping := make(chan struct{})
	go runBotTyping(ctx, bot, channelID, stopTyping)
	reply, aerr := aiBusiness.AnswerChannelQuestion(ctx, userInfo, channelID, channelName, question)
	close(stopTyping)

	if aerr != nil {
		if errors.Is(aerr, ai.ErrRateLimited) || errors.Is(aerr, ai.ErrCircuitOpen) {
			helpers.LogInfoWithContext(ctx, "AI coworker: skipped (throttled/breaker): %v", aerr)
			return
		}
		if errors.Is(aerr, ai.ErrWorkspaceTokenBudgetExceeded) {
			postReply(ctx, bot, chUUID, channelID, postID, botBudgetWorkspaceMessage)
			return
		}
		if errors.Is(aerr, ai.ErrUserTokenBudgetExceeded) {
			postReply(ctx, bot, chUUID, channelID, postID, botBudgetUserMessage)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI coworker: generation failed: %v", aerr)
		postReply(ctx, bot, chUUID, channelID, postID, botFallbackMessage)
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		postReply(ctx, bot, chUUID, channelID, postID, botFallbackMessage)
		return
	}
	postReply(ctx, bot, chUUID, channelID, postID, ai.AppendNoticeFootnote(reply, ai.TakeContextNotice(ctx)))
}

// postReply posts the bot's message as a reply to the @mention. When the
// triggering post id is known it replies as an in-thread comment on that post
// (authored as the bot itself), falling back to a top-level channel message if
// the post id is absent or the comment write fails — so a reply is never lost.
// label is empty so the reply's author is the bot itself (matching the
// mentioned identity).
func postReply(ctx context.Context, bot *userBusiness.BotIdentity, chUUID uuid.UUID, channelID, postID, text string) {
	if strings.TrimSpace(postID) != "" && bot != nil {
		if postUUID, perr := uuid.Parse(postID); perr == nil {
			if _, cerr := botpost.PostCommentToPostAsBot(ctx, postUUID, text, bot); cerr == nil {
				return
			} else {
				helpers.LogErrorWithContext(ctx, "AI coworker: in-thread reply failed on post %s, falling back to channel post: %v", postID, cerr)
			}
		}
	}
	if _, err := botpost.PostToChannel(ctx, chUUID, text, ""); err != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker: failed to post reply in channel %q: %v", channelID, err)
	}
}

// isTransientAIError reports a throttle / open-circuit error, where the coworker
// stays silent (a posted error per blocked turn is noise, and during an outage
// a reply storm). Distinct from a deliberate token cap, which is always
// conveyed.
func isTransientAIError(err error) bool {
	return errors.Is(err, ai.ErrRateLimited) || errors.Is(err, ai.ErrCircuitOpen)
}

// budgetMessageFor maps a token-budget error to the brief, plain in-thread
// message (workspace cap vs the asker's own cap). ok is false for any
// non-budget error.
func budgetMessageFor(err error) (string, bool) {
	switch {
	case errors.Is(err, ai.ErrWorkspaceTokenBudgetExceeded):
		return botBudgetWorkspaceMessage, true
	case errors.Is(err, ai.ErrUserTokenBudgetExceeded):
		return botBudgetUserMessage, true
	default:
		return "", false
	}
}

// ensureCoworkerChannelMembership makes the shared coworker bot a real member of
// a channel it was just @mentioned in, so membership is a queryable fact and the
// AI shows in the roster (Req 8). Checks membership first (cheap) and only adds
// the edge when missing; entirely best-effort so it never blocks a reply.
func ensureCoworkerChannelMembership(ctx context.Context, bot *userBusiness.BotIdentity, chUUID uuid.UUID) {
	if bot == nil || bot.DgraphUID == "" {
		return
	}
	info, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, chUUID, bot.DgraphUID)
	if err != nil || info == nil {
		return
	}
	if info.IsMember != 0 {
		return // already a member
	}
	if aerr := channelBusiness.AddChannelBotMemberEdge(ctx, chUUID, bot.DgraphUID); aerr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker: failed to add bot as channel member: %v", aerr)
	}
}

// handleChatMention answers in a DM or group chat. In a 1:1 DM with the bot it
// replies to EVERY message (the whole conversation is implicitly addressed to
// it); in a group chat it replies only when explicitly @mentioned. In both
// cases the answer is grounded ONLY in that conversation's messages, scoped by
// the asker's permissions, so a group reply can never surface anything the
// other participants couldn't already see.
func handleChatMention(ctx context.Context, data map[string]interface{}) {
	ctx = ai.WithContextNoticeSink(ctx)
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}

	groupID, _ := data["group_id"].(string)
	text, _ := data["text"].(string)
	senderID, _ := data["sender_id"].(string)
	receiverID, _ := data["receiver_id"].(string)
	messageID, _ := data["message_id"].(string)
	if groupID == "" || strings.TrimSpace(text) == "" || senderID == "" {
		return
	}

	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || (bot.UUID == "" && bot.DgraphUID == "") {
		return
	}
	// Never reply to the bot's own messages — also prevents a reply loop, since
	// the bot's reply re-emits chat.created with the bot as sender.
	if senderID == bot.UUID || senderID == bot.DgraphUID {
		return
	}

	// Decide the surface and whether to respond.
	isDM := strings.TrimSpace(receiverID) != ""
	var label string
	if isDM {
		// 1:1 DM: only when the bot is the recipient. Every message there is
		// implicitly addressed to it, so no mention is required.
		if receiverID != bot.UUID && receiverID != bot.DgraphUID {
			// Not the shared coworker. It may be a DM-able specialist agent
			// (Req 10.1): route to that agent's own runner if the recipient is
			// an agent's bot principal. Either way the shared coworker does not
			// answer this DM.
			handleAgentDM(ctx, receiverID, senderID, groupID, text, messageID)
			return
		}
		label = "this direct message"
	} else {
		// Group chat. First, dispatch to any custom agents @mentioned here —
		// independent of the shared coworker (a custom agent can be @mentioned
		// in the group even when the coworker is not). Each mentioned agent runs
		// on its own and replies as an in-thread comment authored as itself.
		dispatchGroupChatAgents(ctx, data, messageID, groupID, senderID, text, nil)

		// Shared coworker: replies only when it is a participant AND explicitly
		// @mentioned. If not, we're done (any custom agents above already ran).
		if !mentionsBot(data["participant_ids"], bot.UUID, bot.DgraphUID) {
			return
		}
		if !mentionsBot(data["mention_ids"], bot.DgraphUID, bot.UUID) {
			return
		}
		label = "this group chat"
	}

	// Admin gate (cheap settings read only after we know we should respond).
	if settings, serr := aiModels.GetSettings(ctx); serr != nil || !settings.Enabled || !settings.CoworkerEnabled {
		if serr != nil {
			helpers.LogErrorWithContext(ctx, "AI coworker: cannot read settings: %v", serr)
		}
		return
	}

	// Asker identity is the permission filter for the conversation-scoped fetch.
	userInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, senderID)
	if err != nil || userInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): cannot resolve sender %q: %v", senderID, err)
		return
	}

	question := cleanQuestion(text, bot.Name)

	// Etiquette guard: don't spend tokens answering a social greeting or a clear
	// stop request. A greeting gets one friendly reply; a dismissal is silent.
	switch agentbusiness.ClassifyMentionIntent(text, nil) {
	case agentbusiness.IntentGreeting:
		if isDM {
			sendBotDM(ctx, bot, senderID, toChatHTML(coworkerGreetingReply()))
		} else {
			postGroupChatReply(ctx, bot, messageID, groupID, senderID, coworkerGreetingReply(), data["participant_ids"])
		}
		return
	case agentbusiness.IntentDismiss:
		return
	}

	// Personal mode (1:1 DM): the full assistant, streamed live into ONE message
	// (workspace memory + RAG + multi-turn continuity + tool awareness), scoped
	// to the asker. Safe to use the asker's full visibility because a 1:1 DM with
	// the bot has no OTHER human who could see leaked context.
	if isDM {
		streamDMReply(ctx, bot, userInfo, groupID, senderID, question)
		return
	}

	// Team mode (group chat): conversation-scoped (AnswerChatQuestion) so a reply
	// can never surface anything the other participants can't already see. Posted
	// as one message (not streamed): a group reply is addressed to everyone,
	// where live editing adds little. The reply lands as an in-thread COMMENT on
	// the triggering message (Slack-style threaded reply) when its id is known.
	stopTyping := make(chan struct{})
	go runBotChatTyping(ctx, bot, groupID, stopTyping)
	answer, aerr := aiBusiness.AnswerChatQuestion(ctx, userInfo, groupID, label, question)
	close(stopTyping)

	if aerr != nil {
		if errors.Is(aerr, ai.ErrRateLimited) || errors.Is(aerr, ai.ErrCircuitOpen) {
			helpers.LogInfoWithContext(ctx, "AI coworker (chat): skipped (throttled/breaker): %v", aerr)
			return
		}
		if errors.Is(aerr, ai.ErrWorkspaceTokenBudgetExceeded) {
			postGroupChatReply(ctx, bot, messageID, groupID, senderID, botBudgetWorkspaceMessage, data["participant_ids"])
			return
		}
		if errors.Is(aerr, ai.ErrUserTokenBudgetExceeded) {
			postGroupChatReply(ctx, bot, messageID, groupID, senderID, botBudgetUserMessage, data["participant_ids"])
			return
		}
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): generation failed: %v", aerr)
		postGroupChatReply(ctx, bot, messageID, groupID, senderID, botFallbackMessage, data["participant_ids"])
		return
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		postGroupChatReply(ctx, bot, messageID, groupID, senderID, botFallbackMessage, data["participant_ids"])
		return
	}
	postGroupChatReply(ctx, bot, messageID, groupID, senderID,
		ai.AppendNoticeFootnote(answer, ai.TakeContextNotice(ctx)), data["participant_ids"])
}

// postGroupChatReply posts the coworker's group-chat answer as an in-thread
// COMMENT on the triggering message (messageID) when it is known — the
// Slack-style threaded reply — falling back to a new group message if the
// message id is absent or the comment write fails, so a reply is never lost.
func postGroupChatReply(ctx context.Context, bot *userBusiness.BotIdentity, messageID, groupID, senderID, text string, rawParticipants interface{}) {
	if strings.TrimSpace(messageID) != "" {
		botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
		if err == nil && botInfo != nil {
			if _, cerr := chatBusiness.PostChatCommentAsBot(ctx, botInfo, messageID, toChatHTML(text)); cerr == nil {
				return
			} else {
				helpers.LogErrorWithContext(ctx, "AI coworker (chat): in-thread group reply failed on msg %s, falling back to group message: %v", messageID, cerr)
			}
		}
	}
	postChatReply(ctx, bot, false, groupID, senderID, text, rawParticipants)
}

// postChatReply writes the bot's reply into the DM (CreateChat, bot -> sender,
// which lands in the same grouping) or the group chat (CreateChatForGroup on
// the existing grouping), authored as the bot.
func postChatReply(ctx context.Context, bot *userBusiness.BotIdentity, isDM bool, groupID, senderID, text string, rawParticipants interface{}) {
	htmlText := toChatHTML(text)
	if isDM {
		sendBotDM(ctx, bot, senderID, htmlText)
		return
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): cannot build bot identity: %v", err)
		return
	}
	participants := resolveParticipantUsers(ctx, rawParticipants)
	if _, cerr := chatBusiness.CreateChatForGroup(ctx, &chatAdapter.ChatInfo{GrpUuid: groupID, TextHtml: htmlText}, botInfo, nil, participants); cerr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): failed to send group reply: %v", cerr)
	}
}

// sendBotDM authors a bot DM (bot -> sender, landing in the same grouping) with
// a pre-rendered HTML body. Shared by the plain-text reply path (postChatReply)
// and the source-cited fallback reply, so DM authoring (bot identity + sender
// resolution + CreateChat) lives in exactly one place.
func sendBotDM(ctx context.Context, bot *userBusiness.BotIdentity, senderID, htmlText string) {
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): cannot build bot identity: %v", err)
		return
	}
	toUUID, perr := uuid.Parse(senderID)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): bad sender uuid %q: %v", senderID, perr)
		return
	}
	senderDgraph, derr := userBusiness.GetDgraphUserInfoByUUID(ctx, senderID)
	if derr != nil || senderDgraph == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): cannot resolve sender dgraph %q: %v", senderID, derr)
		return
	}
	if _, cerr := chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{ToUuid: senderID, TextHtml: htmlText}, botInfo, senderDgraph, toUUID, nil); cerr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): failed to send DM reply: %v", cerr)
	}
}

// resolveParticipantUsers turns the event's participant uuids into Dgraph users
// for CreateChatForGroup (used for its last-seen, notifications, and search
// indexing). Best-effort: a resolution miss just means a degraded index entry,
// never a failed reply.
func resolveParticipantUsers(ctx context.Context, raw interface{}) []*dgraphStruct.DgraphUser {
	uuids := toStringSlice(raw)
	if len(uuids) == 0 {
		return nil
	}
	// By their uuids: GetDgraphUserInfoByUUIDs takes graph uids, and refused
	// these, so the reply's participants were always empty.
	users, err := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, uuids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): resolve participants failed: %v", err)
		return nil
	}
	return users
}

// toStringSlice coerces an event field (which may be []string or
// []interface{}) into a []string.
func toStringSlice(raw interface{}) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// toChatHTML renders the model's plain-text answer as safe chat HTML: each
// paragraph wrapped in <p>, single newlines as <br>. The text is escaped first,
// so nothing the model emits can inject markup.
func toChatHTML(text string) string {
	// Delegate to the shared agent-body renderer so a ```chart block posted in a
	// DM or group chat renders as an inline chart embed, exactly as it does in a
	// channel post. It escapes all other text (nothing the model emits can
	// inject markup) and returns "<p></p>" for empty input.
	return botpost.RenderAgentBodyHTML(text)
}

// runBotChatTyping publishes a chat typing indicator as the bot and keeps it
// alive until stop is closed (or ctx is cancelled).
func runBotChatTyping(ctx context.Context, bot *userBusiness.BotIdentity, grpID string, stop <-chan struct{}) {
	publishBotChatTyping(bot, grpID)
	t := time.NewTicker(typingRepublishInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			publishBotChatTyping(bot, grpID)
		}
	}
}

func publishBotChatTyping(bot *userBusiness.BotIdentity, grpID string) {
	mqttBusiness.PublishChatTyping(&mqttStruct.MqttChatTyping{
		UserName:    bot.Name,
		UserUUID:    bot.UUID,
		UserProfile: bot.ProfileKey,
		ChatGrpId:   grpID,
	}, grpID)
}

// runBotTyping publishes a channel typing indicator as the bot and keeps it
// alive until stop is closed (or ctx is cancelled).
func runBotTyping(ctx context.Context, bot *userBusiness.BotIdentity, channelID string, stop <-chan struct{}) {
	publishBotTyping(bot, channelID)
	t := time.NewTicker(typingRepublishInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			publishBotTyping(bot, channelID)
		}
	}
}

func publishBotTyping(bot *userBusiness.BotIdentity, channelID string) {
	mqttBusiness.PublishChannelTyping(&mqttStruct.MqttChannelTyping{
		UserName:    bot.Name,
		UserUUID:    bot.UUID,
		UserProfile: bot.ProfileKey,
		ChannelUuid: channelID,
	}, channelID)
}

// mentionsBot reports whether any of botIDs appears in the event's mention ids.
// The payload is []string but arrives as interface{} through the generic event
// map, so handle both shapes defensively.
func mentionsBot(raw interface{}, botIDs ...string) bool {
	match := func(s string) bool {
		for _, id := range botIDs {
			if id != "" && s == id {
				return true
			}
		}
		return false
	}
	switch v := raw.(type) {
	case []string:
		for _, m := range v {
			if match(m) {
				return true
			}
		}
	case []interface{}:
		for _, m := range v {
			if s, ok := m.(string); ok && match(s) {
				return true
			}
		}
	}
	return false
}

// createDMPendingActions persists each write the assistant proposed in a 1:1 DM
// as a durable, approvable action (surviving tab close / reload) and returns a
// short in-thread note listing them. The bot itself takes no action: each
// proposal becomes an Approve/Deny card that, when approved, executes AS the
// requesting human with their permissions re-checked (no confused-deputy).
// Returns "" when there are no proposed actions.
func createDMPendingActions(ctx context.Context, senderID, groupID string, actions []aiAdapter.ProposedAction) string {
	if len(actions) == 0 {
		return ""
	}
	requestedBy, perr := uuid.Parse(senderID)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): bad sender uuid for pending action %q: %v", senderID, perr)
		return ""
	}

	descs := make([]string, 0, len(actions))
	for _, a := range actions {
		d := strings.TrimSpace(a.Description)
		if d == "" {
			d = strings.TrimSpace(a.ToolName)
		}
		if _, cerr := aiBusiness.CreatePendingAction(ctx, requestedBy, "dm", groupID, a.ToolName, a.Params, d, ""); cerr != nil {
			helpers.LogErrorWithContext(ctx, "AI coworker (chat): failed to create pending action for %q: %v", a.ToolName, cerr)
			continue
		}
		if d != "" {
			descs = append(descs, "• "+d)
		}
	}
	if len(descs) == 0 {
		return ""
	}
	return "I can take this for you — approve to run it:\n" + strings.Join(descs, "\n")
}

// sourceLabel turns one source reference into a short human label. Channel
// messages cite their channel by name; other kinds cite their type (we avoid
// echoing raw content/snippets into a public-ish reply).
func sourceLabel(s aiAdapter.SourceRef) string {
	switch s.ContentType {
	case "post", "comment":
		if name := strings.TrimSpace(s.ChannelName); name != "" {
			return "#" + name
		}
		return "a channel message"
	case "chat":
		if strings.Contains(s.ChatGrpID, " ") {
			return "a direct message"
		}
		return "a group chat"
	case "doc":
		return "a doc"
	case "task":
		return "a task"
	default:
		return ""
	}
}

// sourcesFooterHTML renders the de-duplicated "Sources:" line as a row of
// CLICKABLE internal deep links — the chat analog of the assistant panel's
// SourceList, so a citation opens its real content with one tap. It mirrors the
// FE `sourceHref` routing exactly (channel/doc/task/chat) and emits relative
// /app paths the message renderer resolves through client-side navigation;
// any source we can't build a precise link for is still listed as plain text so
// the footer stays complete. Returns "" when there are no usable sources. The
// answer body is rendered separately (escaped) and this footer is appended as
// trusted markup, so nothing the model emits is interpolated into an href.
// askerID scopes a 1:1 DM source to the OTHER participant. Capped at 5 so the
// footer stays a one-liner.
func sourcesFooterHTML(sources []aiAdapter.SourceRef, askerID string) string {
	if len(sources) == 0 {
		return ""
	}
	seen := map[string]bool{}
	parts := make([]string, 0, 5)
	for _, s := range sources {
		label := sourceLabel(s)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		escapedLabel := html.EscapeString(label)
		if href := sourceDeepLink(s, askerID); href != "" {
			parts = append(parts, `<a href="`+html.EscapeString(href)+`" class="link">`+escapedLabel+`</a>`)
		} else {
			parts = append(parts, escapedLabel)
		}
		if len(parts) >= 5 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "<p>Sources: " + strings.Join(parts, ", ") + "</p>"
}

// sourceDeepLink builds the internal /app deep link for a cited source,
// mirroring the FE `sourceHref` paths so a citation opens the same target the
// assistant panel would. Returns "" when no precise target can be built (the
// caller then lists the source as plain text rather than a dead link).
func sourceDeepLink(s aiAdapter.SourceRef, askerID string) string {
	switch s.ContentType {
	case "post":
		if s.ChannelUUID != "" {
			if s.ContentUUID != "" {
				return "/app/channel/" + s.ChannelUUID + "/" + s.ContentUUID
			}
			return "/app/channel/" + s.ChannelUUID
		}
	case "comment":
		// A comment isn't separately addressable — route to its parent.
		if s.PostUUID != "" && s.ChannelUUID != "" {
			return "/app/channel/" + s.ChannelUUID + "/" + s.PostUUID
		}
		if s.TaskUUID != "" {
			return "/app/task/" + s.TaskUUID
		}
		if s.DocUUID != "" {
			return "/app/doc/" + s.DocUUID + "/comment"
		}
	case "doc":
		if s.ContentUUID != "" {
			return "/app/doc/" + s.ContentUUID
		}
	case "task":
		if s.ContentUUID != "" {
			return "/app/task/" + s.ContentUUID
		}
	case "chat":
		return chatDeepLink(s, askerID)
	}
	return ""
}

// chatDeepLink routes a chat source to the right conversation, mirroring the FE
// `chatHref`: a group chat uses its 32-char grouping id (no space); a 1:1 DM
// encodes both user uuids separated by a space, so it routes to the OTHER
// participant relative to the asker. Returns "" when the target can't be built.
func chatDeepLink(s aiAdapter.SourceRef, askerID string) string {
	grp := strings.TrimSpace(s.ChatGrpID)
	if grp == "" {
		return ""
	}
	if !strings.Contains(grp, " ") {
		return "/app/chat/group/" + grp + "/" + s.ContentUUID
	}
	other := otherParticipant(grp, askerID)
	if other == "" {
		return ""
	}
	return "/app/chat/" + other + "/" + s.ContentUUID
}

// otherParticipant returns the first uuid in a DM grouping id ("uuidA uuidB")
// that is not selfID, i.e. the conversation partner. "" if none.
func otherParticipant(grp, selfID string) string {
	for _, p := range strings.Fields(grp) {
		if p != selfID {
			return p
		}
	}
	return ""
}

// cleanQuestion strips a leading bot @mention/handle from the plain-text
// harmless. Falls back to a generic prompt if nothing meaningful remains.
func cleanQuestion(text, botName string) string {
	q := strings.TrimSpace(text)
	// Strip a leading bot handle/name so the model sees a clean prompt. Covers
	// the live display name + username and the legacy "automation" identity, so
	// it stays correct across a rename and in not-yet-migrated workspaces.
	tokens := []string{
		"@" + botName, botName,
		"@onecamp-ai", "onecamp-ai",
		"@OneCamp AI", "OneCamp AI",
		"@OneCamp Automation", "OneCamp Automation",
		"@automation", "automation",
	}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(q), strings.ToLower(token)) {
			q = strings.TrimSpace(q[len(token):])
			q = strings.TrimPrefix(q, ":")
			q = strings.TrimSpace(q)
			break
		}
	}
	if q == "" {
		return "Based on this channel's recent activity, what should I know?"
	}
	return q
}

// handleIssueTriage runs the code-aware bug agent on a newly-opened GitHub
// issue and posts the proposed root-cause + fix as an internal comment on the
// auto-created task. Opt-in (ai_settings.issue_triage_enabled) because it spends
// an LLM call per opened issue. Nothing is pushed back to GitHub.
func handleIssueTriage(ctx context.Context, data map[string]interface{}) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}

	settings, serr := aiModels.GetSettings(ctx)
	if serr != nil || !settings.Enabled || !settings.IssueTriageEnabled {
		return
	}

	owner, _ := data["owner"].(string)
	repo, _ := data["repo"].(string)
	title, _ := data["title"].(string)
	body, _ := data["body"].(string)
	taskUUIDStr, _ := data["task_uuid"].(string)
	createdBy, _ := data["created_by"].(string)
	if owner == "" || repo == "" || taskUUIDStr == "" {
		return
	}

	taskUUID, perr := uuid.Parse(taskUUIDStr)
	if perr != nil {
		return
	}

	analysis, aerr := codeagent.AnalyzeIssue(ctx, owner, repo, title, body, "", false)
	if aerr != nil {
		if errors.Is(aerr, ai.ErrRateLimited) || errors.Is(aerr, ai.ErrCircuitOpen) {
			helpers.LogInfoWithContext(ctx, "AI issue triage: skipped (throttled/breaker): %v", aerr)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI issue triage: analysis failed: %v", aerr)
		return
	}
	if strings.TrimSpace(analysis.Answer) == "" {
		return
	}

	// Post the analysis as the automation bot. Resolve the bot's full UserInfo
	// (comment author) and the task's Dgraph node (read as the bot).
	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || bot.UUID == "" {
		return
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI issue triage: cannot build bot identity: %v", err)
		return
	}
	dgraphTask, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, botInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphTask == nil {
		// Fall back to the link creator's view if the bot can't see the task.
		if createdBy != "" {
			if cu, cerr := aiBusiness.BuildUserInfoByUserUUID(ctx, createdBy); cerr == nil && cu != nil {
				dgraphTask, err = taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, cu.UserDgraphInfo.Uid)
			}
		}
		if err != nil || dgraphTask == nil {
			helpers.LogErrorWithContext(ctx, "AI issue triage: cannot load task %s: %v", taskUUIDStr, err)
			return
		}
	}

	commentInput := &taskAdapter.CreateOrUpdateTaskCommentInput{
		CommentBody:    analysisCommentHTML(analysis.Answer, analysis.Partial),
		TaskUuid:       taskUUIDStr,
		SkipGitHubSync: true, // internal only — never echo back to the GitHub issue
	}
	if _, cerr := taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTask, botInfo, commentInput, nil); cerr != nil {
		helpers.LogErrorWithContext(ctx, "AI issue triage: failed to post task comment: %v", cerr)
		return
	}
	helpers.LogInfoWithContext(ctx, "AI issue triage: posted analysis on task %s", taskUUIDStr)
}

// analysisCommentHTML renders the agent's markdown answer as safe task-comment
// HTML: fenced ```code``` blocks become <pre><code> (preserving the diff),
// everything else becomes escaped paragraphs. A header marks it as AI-authored,
// and a partial-retrieval caveat is appended when relevant.
func analysisCommentHTML(answer string, partial bool) string {
	var b strings.Builder
	b.WriteString("<p><strong>AI bug analysis</strong></p>")

	segments := strings.Split(answer, "```")
	for i, seg := range segments {
		if i%2 == 1 {
			// Inside a fenced block: drop an optional language token on line 1.
			code := seg
			if nl := strings.IndexByte(code, '\n'); nl >= 0 {
				firstLine := strings.TrimSpace(code[:nl])
				if firstLine != "" && !strings.ContainsAny(firstLine, " \t") {
					code = code[nl+1:]
				}
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.TrimRight(code, "\n")))
			b.WriteString("</code></pre>")
			continue
		}
		for _, para := range strings.Split(seg, "\n\n") {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
			b.WriteString("<p>")
			b.WriteString(strings.ReplaceAll(html.EscapeString(para), "\n", "<br/>"))
			b.WriteString("</p>")
		}
	}

	if partial {
		b.WriteString("<p><em>Note: this is a large repository and only part of it could be reviewed, so the proposed fix may be incomplete.</em></p>")
	}
	return b.String()
}

// handlePRReview runs the code agent's PR review on a newly-opened pull request
// and posts the feedback as an internal comment on the linked task. Shares the
// issue_triage_enabled opt-in (one "GitHub auto-review" control covers issues
// and PRs). Read-only against GitHub.
func handlePRReview(ctx context.Context, data map[string]interface{}) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}
	settings, serr := aiModels.GetSettings(ctx)
	if serr != nil || !settings.Enabled || !settings.IssueTriageEnabled {
		return
	}

	owner, _ := data["owner"].(string)
	repo, _ := data["repo"].(string)
	title, _ := data["title"].(string)
	body, _ := data["body"].(string)
	taskUUIDStr, _ := data["task_uuid"].(string)
	createdBy, _ := data["created_by"].(string)
	prNumber := toInt(data["pr_number"])
	if owner == "" || repo == "" || taskUUIDStr == "" || prNumber <= 0 {
		return
	}

	taskUUID, perr := uuid.Parse(taskUUIDStr)
	if perr != nil {
		return
	}

	review, rerr := codeagent.ReviewPullRequest(ctx, owner, repo, prNumber, title, body)
	if rerr != nil {
		if errors.Is(rerr, ai.ErrRateLimited) || errors.Is(rerr, ai.ErrCircuitOpen) {
			helpers.LogInfoWithContext(ctx, "AI PR review: skipped (throttled/breaker): %v", rerr)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI PR review: failed: %v", rerr)
		return
	}
	if strings.TrimSpace(review.Answer) == "" {
		return
	}

	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || bot.UUID == "" {
		return
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		helpers.LogErrorWithContext(ctx, "AI PR review: cannot build bot identity: %v", err)
		return
	}
	dgraphTask, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, botInfo.UserDgraphInfo.Uid)
	if (err != nil || dgraphTask == nil) && createdBy != "" {
		if cu, cerr := aiBusiness.BuildUserInfoByUserUUID(ctx, createdBy); cerr == nil && cu != nil {
			dgraphTask, err = taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, cu.UserDgraphInfo.Uid)
		}
	}
	if err != nil || dgraphTask == nil {
		helpers.LogErrorWithContext(ctx, "AI PR review: cannot load task %s: %v", taskUUIDStr, err)
		return
	}

	commentInput := &taskAdapter.CreateOrUpdateTaskCommentInput{
		CommentBody:    prReviewCommentHTML(review.Answer, review.Partial),
		TaskUuid:       taskUUIDStr,
		SkipGitHubSync: true,
	}
	if _, cerr := taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTask, botInfo, commentInput, nil); cerr != nil {
		helpers.LogErrorWithContext(ctx, "AI PR review: failed to post task comment: %v", cerr)
		return
	}
	helpers.LogInfoWithContext(ctx, "AI PR review: posted review on task %s (PR #%d)", taskUUIDStr, prNumber)
}

// prReviewCommentHTML renders the PR review with an "AI code review" header,
// reusing the same safe markdown->HTML conversion as issue triage.
func prReviewCommentHTML(answer string, partial bool) string {
	html := analysisCommentHTML(answer, partial)
	// Swap the generic header for a PR-specific one.
	return strings.Replace(html, "<p><strong>AI bug analysis</strong></p>", "<p><strong>AI code review</strong></p>", 1)
}

// toInt coerces an event field (json number arrives as float64, or may be int)
// into an int.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
