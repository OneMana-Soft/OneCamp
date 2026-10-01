package aicoworker

// Streamed 1:1 DM replies: the full-assistant answer (AskAIStream) types into a
// single DM message live, the chat analog of the channel mention streamer. It
// orchestrates the existing, tested chat primitives — CreateChat for the
// message, MqttChat TYPE_UPDATE edits for the live typing, UpdateChat for the
// persisted final — so there is no reimplementation of chat persistence and the
// FE renders it through the same edit path it already uses for an edited
// message (no FE change).
//
// The message is created lazily on the FIRST streamed token, so a run blocked
// before any content (throttle/breaker, which AskAI checks up front) leaves no
// placeholder and the DM simply stays silent.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	// dmStreamCursor reads as the bot actively typing in the live message.
	dmStreamCursor = "▍"
	// dmStreamThrottle bounds how often a streaming DM reply republishes its
	// edit so a fast token stream cannot flood MQTT / clients. The final text is
	// always written on Finalize regardless of throttle.
	dmStreamThrottle = 200 * time.Millisecond
)

// streamDMReply produces the DM assistant answer with AskAIStream and types it
// live into one message via a dmStream, then finalizes it (persisted). A typing
// indicator covers the gap before the first token; once content flows the live
// message is the indicator. Error semantics match the non-streamed path: silent
// on transient throttle/breaker (no message), an explainable note on a
// token-budget pause, a brief honest note otherwise. Falls back to a one-shot
// reply if the stream cannot be initialized.
func streamDMReply(ctx context.Context, bot *userBusiness.BotIdentity, userInfo *userModels.UserInfo, groupID, senderID, question string) {
	dm, derr := newDMStream(ctx, bot, groupID, senderID)
	if derr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM stream init failed, falling back: %v", derr)
		streamlessDMReply(ctx, bot, userInfo, groupID, senderID, question)
		return
	}

	stopTyping := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopTyping) }) }
	go runBotChatTyping(ctx, bot, groupID, stopTyping)

	resp, aerr := aiBusiness.AskAIStream(ctx, userInfo, question, "dm:"+groupID, nil, func(running string) {
		stop() // first content: the live message itself is now the indicator
		dm.Push(ctx, running)
	})
	stop()

	if aerr != nil {
		if isTransientAIError(aerr) {
			helpers.LogInfoWithContext(ctx, "AI coworker (chat): DM skipped (throttled/breaker): %v", aerr)
			dm.Discard(ctx)
			return
		}
		if msg, ok := budgetMessageFor(aerr); ok {
			dm.Finalize(ctx, msg)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM generation failed: %v", aerr)
		dm.Finalize(ctx, botFallbackMessage)
		return
	}

	answer := ""
	var sources []aiAdapter.SourceRef
	if resp != nil {
		answer = strings.TrimSpace(resp.Answer)
		sources = resp.Sources
		if note := createDMPendingActions(ctx, senderID, groupID, resp.ProposedActions); note != "" {
			answer = strings.TrimSpace(answer + "\n\n" + note)
		}
	}
	if answer == "" {
		dm.Finalize(ctx, botFallbackMessage)
		return
	}
	// Render the answer (escaped) and append the clickable "Sources:" footer as
	// trusted markup, so a citation deep-links to its content. When there are no
	// linkable sources the footer is empty and we use the plain-text path.
	if footerHTML := sourcesFooterHTML(sources, senderID); footerHTML != "" {
		dm.FinalizeHTML(ctx, toChatHTML(answer)+footerHTML)
		return
	}
	dm.Finalize(ctx, answer)
}

// streamlessDMReply is the original non-streamed DM path, kept as a fallback for
// the rare case the stream cannot be initialized.
func streamlessDMReply(ctx context.Context, bot *userBusiness.BotIdentity, userInfo *userModels.UserInfo, groupID, senderID, question string) {
	stopTyping := make(chan struct{})
	go runBotChatTyping(ctx, bot, groupID, stopTyping)
	resp, aerr := aiBusiness.AskAI(ctx, userInfo, question, "dm:"+groupID, nil)
	close(stopTyping)

	if aerr != nil {
		if isTransientAIError(aerr) {
			helpers.LogInfoWithContext(ctx, "AI coworker (chat): DM skipped (throttled/breaker): %v", aerr)
			return
		}
		if msg, ok := budgetMessageFor(aerr); ok {
			postChatReply(ctx, bot, true, groupID, senderID, msg, nil)
			return
		}
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM generation failed: %v", aerr)
		postChatReply(ctx, bot, true, groupID, senderID, botFallbackMessage, nil)
		return
	}
	answer := ""
	var sources []aiAdapter.SourceRef
	if resp != nil {
		answer = strings.TrimSpace(resp.Answer)
		sources = resp.Sources
		if note := createDMPendingActions(ctx, senderID, groupID, resp.ProposedActions); note != "" {
			answer = strings.TrimSpace(answer + "\n\n" + note)
		}
	}
	if answer == "" {
		postChatReply(ctx, bot, true, groupID, senderID, botFallbackMessage, nil)
		return
	}
	// Match the streamed path: cite sources as clickable deep links when we can
	// build them, otherwise send the plain answer.
	if footerHTML := sourcesFooterHTML(sources, senderID); footerHTML != "" {
		sendBotDM(ctx, bot, senderID, toChatHTML(answer)+footerHTML)
		return
	}
	postChatReply(ctx, bot, true, groupID, senderID, answer, nil)
}

// dmStream types an assistant reply into one DM message: lazily create the chat
// on the first token, republish throttled live edits, then persist the final.
type dmStream struct {
	bot          *userBusiness.BotIdentity
	botInfo      *userModels.UserInfo
	senderDgraph *dgraphStruct.DgraphUser
	toUUID       uuid.UUID
	senderID     string
	groupID      string

	mu          sync.Mutex
	chatUUID    uuid.UUID
	created     bool
	finalized   bool
	lastPublish time.Time
	lastHTML    string
}

// newDMStream resolves the bot's full identity and the recipient, the same
// inputs CreateChat needs to author a DM as the bot.
func newDMStream(ctx context.Context, bot *userBusiness.BotIdentity, groupID, senderID string) (*dmStream, error) {
	if bot == nil || bot.UUID == "" {
		return nil, fmt.Errorf("bot identity not available")
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		return nil, fmt.Errorf("cannot build bot identity: %w", err)
	}
	senderDgraph, derr := userBusiness.GetDgraphUserInfoByUUID(ctx, senderID)
	if derr != nil || senderDgraph == nil {
		return nil, fmt.Errorf("cannot resolve sender %q: %w", senderID, derr)
	}
	toUUID, perr := uuid.Parse(senderID)
	if perr != nil {
		return nil, fmt.Errorf("bad sender uuid %q: %w", senderID, perr)
	}
	return &dmStream{
		bot:          bot,
		botInfo:      botInfo,
		senderDgraph: senderDgraph,
		toUUID:       toUUID,
		senderID:     senderID,
		groupID:      groupID,
	}, nil
}

// Push renders the running answer into the live message: the first non-empty
// call creates the DM message; subsequent calls republish it as a throttled
// live edit. Called sequentially from the stream loop.
func (s *dmStream) Push(ctx context.Context, running string) {
	running = strings.TrimSpace(running)
	if running == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}
	if !s.created {
		s.create(ctx, toChatHTML(running+dmStreamCursor))
		return
	}
	now := time.Now()
	if now.Sub(s.lastPublish) < dmStreamThrottle {
		return
	}
	s.publishEdit(toChatHTML(running + dmStreamCursor))
	s.lastPublish = now
}

// Finalize writes the complete answer as the message's final text (persisted via
// UpdateChat, which also broadcasts the edit + indexes it). If nothing streamed
// (single-shot), it creates the message with the final text. Idempotent.
func (s *dmStream) Finalize(ctx context.Context, finalText string) {
	s.FinalizeHTML(ctx, toChatHTML(strings.TrimSpace(finalText)))
}

// FinalizeHTML is Finalize for a body that is already safe HTML (e.g. the plain
// answer rendered by toChatHTML with a clickable "Sources:" footer appended).
// It skips the plain-text escaping Finalize applies, so the footer's anchors
// survive. Same persistence + idempotency semantics as Finalize.
func (s *dmStream) FinalizeHTML(ctx context.Context, finalHTML string) {
	finalHTML = strings.TrimSpace(finalHTML)
	if finalHTML == "" {
		finalHTML = toChatHTML("")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}
	s.finalized = true

	if !s.created {
		s.create(ctx, finalHTML)
		return
	}
	if err := chatBusiness.UpdateChat(ctx, &chatAdapter.ChatInfo{Uuid: s.chatUUID.String(), TextHtml: finalHTML}, s.chatUUID, s.groupID, s.bot.UUID, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM stream finalize failed: %v", err)
		s.publishEdit(finalHTML) // best-effort live update even if the persist failed
	}
}

// Discard abandons a stream with no answer. If nothing was posted (the common
// throttle/breaker case, which AskAI catches before any token) it stays silent;
// if a partial message was already posted (rare), it is replaced with the
// honest fallback note rather than left as a stuck "typing" placeholder.
func (s *dmStream) Discard(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return
	}
	s.finalized = true
	if !s.created {
		return
	}
	finalHTML := toChatHTML(botFallbackMessage)
	if err := chatBusiness.UpdateChat(ctx, &chatAdapter.ChatInfo{Uuid: s.chatUUID.String(), TextHtml: finalHTML}, s.chatUUID, s.groupID, s.bot.UUID, nil); err != nil {
		s.publishEdit(finalHTML)
	}
}

// create authors the DM message as the bot via CreateChat (bot -> sender, which
// lands in the same grouping) and records its uuid. Caller holds s.mu.
func (s *dmStream) create(ctx context.Context, htmlText string) {
	created, err := chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{ToUuid: s.senderID, TextHtml: htmlText}, s.botInfo, s.senderDgraph, s.toUUID, nil)
	if err != nil || created == nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM stream create failed: %v", err)
		return
	}
	cu, perr := uuid.Parse(created.Uuid)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "AI coworker (chat): DM stream bad chat uuid %q: %v", created.Uuid, perr)
		return
	}
	s.chatUUID = cu
	s.created = true
	s.lastHTML = htmlText
	s.lastPublish = time.Now()
}

// publishEdit broadcasts a live TYPE_UPDATE edit of the streaming message
// (no DB write — Finalize persists). Caller holds s.mu.
func (s *dmStream) publishEdit(htmlText string) {
	if htmlText == s.lastHTML {
		return
	}
	s.lastHTML = htmlText
	now := time.Now()
	mqttBusiness.PublishChat(&mqttStruct.MqttChat{
		Type:           mqttStruct.TYPE_UPDATE,
		ChatUuid:       s.chatUUID.String(),
		ChatHtmlText:   htmlText,
		ChatGrpId:      s.groupID,
		ChatByUserUuid: s.bot.UUID,
		ChatUpdatedAt:  &now,
	}, s.groupID)
}
