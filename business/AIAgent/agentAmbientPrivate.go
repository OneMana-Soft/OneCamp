package business

// Speak, stay silent, or tell the sponsor privately.
//
// An agent that follows a channel on its own used to have two choices: reply in
// the thread, or say nothing. The judgement people actually want from a
// colleague has a third: "that is worth your knowing, but not worth saying in
// front of everyone." It is also the governed answer to the risk reviewers
// raise with ambient agents (Ando, September 2026): an agent may lawfully read
// two channels and still leak one into the other by replying. So a public
// ambient reply draws on this channel, and anything that draws on somewhere
// else, or needs the sponsor's decision first, goes to the sponsor alone, with
// a link back to the message.

import (
	"context"
	"fmt"
	"html"
	"strings"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// relatedElsewhereMax bounds what the agent is shown from other channels.
const relatedElsewhereMax = 3

// relatedSearch finds discussions related to a message, as the sponsor sees
// the workspace (a seam; the agent never sees more than its sponsor could).
var relatedSearch = func(ctx context.Context, sponsor uuid.UUID, text string) ([]aiBusiness.UnifiedHit, error) {
	info, err := aiBusiness.BuildUserInfoByUserUUID(ctx, sponsor.String())
	if err != nil || info == nil {
		return nil, fmt.Errorf("sponsor: %v", err)
	}
	resp, err := aiBusiness.UnifiedSearch(ctx, info, text)
	if err != nil || resp == nil || !resp.Enabled {
		return nil, err
	}
	var hits []aiBusiness.UnifiedHit
	for _, g := range resp.Groups {
		hits = append(hits, g.Hits...)
	}
	return hits, nil
}

// relatedElsewhere keeps the hits that are channel conversations in OTHER
// channels (never a DM, never this channel), as one line each. Pure.
func relatedElsewhere(hits []aiBusiness.UnifiedHit, channelID string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hits {
		if len(out) == max {
			break
		}
		if h.ChannelUUID == "" || h.ChannelUUID == channelID || h.ChatGrpID != "" {
			continue
		}
		snippet := strings.Join(strings.Fields(h.Snippet), " ")
		if snippet == "" || seen[snippet] {
			continue
		}
		seen[snippet] = true
		if r := []rune(snippet); len(r) > ambientTriggerQuoteRunes {
			snippet = string(r[:ambientTriggerQuoteRunes]) + "…"
		}
		where := "#" + strings.TrimPrefix(strings.TrimSpace(h.ChannelName), "#")
		if where == "#" {
			where = "another channel"
		}
		out = append(out, where+": "+snippet)
	}
	return out
}

// ambientPrivatePrefix starts a reply meant only for the sponsor.
const ambientPrivatePrefix = "PRIVATE:"

// ambientTriggerQuoteRunes bounds how much of the message is quoted back.
const ambientTriggerQuoteRunes = 160

// ambientDecision reads an ambient run's reply: something to post in the
// thread, something to tell the sponsor privately, or neither. Pure.
func ambientDecision(outcome *RunOutcome) (public, private string) {
	text, ok := scheduledCheckinText(outcome)
	if !ok {
		return "", ""
	}
	if i := strings.Index(strings.ToUpper(text), ambientPrivatePrefix); i == 0 {
		note := strings.TrimSpace(text[len(ambientPrivatePrefix):])
		return "", note
	}
	return text, ""
}

// ambientPrivateNoteHTML is the sponsor's message: the note, the channel
// message it is about (quoted briefly), and a link to it. Pure.
func ambientPrivateNoteHTML(channelID, postID, trigger, note string) string {
	quote := []rune(strings.Join(strings.Fields(trigger), " "))
	if len(quote) > ambientTriggerQuoteRunes {
		quote = append(quote[:ambientTriggerQuoteRunes], '…')
	}
	return fmt.Sprintf(`<p>%s</p><blockquote>%s</blockquote><p><a href="/app/channel/%s/%s">Open the message</a></p>`,
		html.EscapeString(note), html.EscapeString(string(quote)),
		html.EscapeString(channelID), html.EscapeString(postID))
}

// Seam for tests.
var sendAgentDM = deliverAgentDM

// deliverAgentDM sends a direct message from an agent's bot to one person.
func deliverAgentDM(ctx context.Context, bot *userBusiness.BotIdentity, toUserID uuid.UUID, htmlText string) error {
	if bot == nil {
		return fmt.Errorf("agent has no bot identity")
	}
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		return fmt.Errorf("bot identity: %v", err)
	}
	to := toUserID.String()
	toDgraph, err := userBusiness.GetDgraphUserInfoByUUID(ctx, to)
	if err != nil || toDgraph == nil {
		return fmt.Errorf("recipient: %v", err)
	}
	if _, err := chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{ToUuid: to, TextHtml: htmlText}, botInfo, toDgraph, toUserID, nil); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// deliverAmbientOutcome acts on an ambient run's decision.
func deliverAmbientOutcome(ctx context.Context, a *model.AiAgent, bot *userBusiness.BotIdentity, channelID, postID, trigger string, outcome *RunOutcome) {
	public, private := ambientDecision(outcome)
	switch {
	case public != "":
		postAgentReply(ctx, a, bot, channelID, postID, public)
	case private != "":
		if err := sendAgentDM(ctx, bot, a.CreatedBy, ambientPrivateNoteHTML(channelID, postID, trigger, private)); err != nil {
			helpers.LogErrorWithContext(ctx, "agentAmbient: private note to sponsor failed (agent=%s): %v", a.Id, err)
		}
	}
}
