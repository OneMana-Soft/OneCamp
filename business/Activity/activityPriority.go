package business

// Activity priority scoring.
//
// WHY DETERMINISTIC (NOT AN LLM CALL PER ITEM)
// --------------------------------------------
// Whether a notification deserves the user's immediate attention is mostly a
// STRUCTURAL signal: a direct @mention that asks a question or requests an
// action is high-priority; a plain mention or a comment on your content is
// normal; a reaction to your content is low. Scoring every activity item
// with an LLM would be exactly the per-event model storm the rest of the
// system avoids — catastrophic cost/latency on a local Ollama and adds
// little over good heuristics. This scorer is instant, free, allocation-
// light, and runs inline in GetUnifiedActivity so the feed can offer a
// "Priority" view with zero new infrastructure.
//
// The output is a coarse, honest 3-level signal (high/normal/low) the FE
// uses to sort/filter — never to SUPPRESS anything (we never hide a
// notification the user would otherwise see; we only re-order/triage).

import (
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// Priority levels (mirrored as strings on UnifiedActivityItem.Priority and
// in the FE).
const (
	PriorityHigh   = "high"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// actionCues are phrases that signal the message wants something FROM the
// reader — a reply, a decision, or work. A direct mention containing one of
// these is treated as high priority. Kept lowercase; matched on lowered text.
var actionCues = []string{
	"can you", "could you", "would you", "can u", "pls", "please",
	"need", "needs", "blocker", "blocked", "asap", "urgent", "review",
	"approve", "approval", "sign off", "sign-off", "take a look", "thoughts?",
	"waiting on", "waiting for", "follow up", "follow-up", "action required",
	"by today", "by eod", "by tomorrow", "deadline", "help",
}

// approvalAsked is the notice an approver gets when a request asks for their
// answer (business/Approvals tellApprovers).
const approvalAsked = "work.approval.requested"

// ScorePriority assigns a coarse priority to one activity item, the feed's
// and a live notice's alike. It is pure and side-effect free. Heuristics, in
// order of strength:
//   - MENTION of the user that asks a question or carries an action cue → high
//   - MENTION (plain) → normal
//   - COMMENT on the user's content that asks a question / action cue → high
//   - COMMENT (plain) → normal
//   - NOTICE of an approval request asking for the user's answer → high
//   - NOTICE (any other) → normal
//   - REACTION → low (nice-to-know, rarely actionable)
//
// What the user has since answered drops to low: demoteAnswered.
func ScorePriority(item dgraphModels.UnifiedActivityItem) string {
	switch item.ActivityType {
	case "MENTION":
		if item.Mention == nil {
			return PriorityNormal
		}
		text := mentionText(item.Mention)
		if isActionable(text) {
			return PriorityHigh
		}
		// A direct mention is inherently more salient than a passive
		// comment/reaction even without an explicit ask.
		return PriorityNormal

	case "COMMENT":
		if item.Comment != nil && isActionable(helpers.HTMLToPlainText(item.Comment.Text)) {
			return PriorityHigh
		}
		return PriorityNormal

	case "REACTION":
		return PriorityLow

	case "NOTICE":
		if approvalOf(&item) != "" {
			return PriorityHigh
		}
		return PriorityNormal

	default:
		return PriorityNormal
	}
}

// mentionText extracts the human text from whichever surface a mention
// points at (chat / post / comment). Tasks and docs carry no inline body
// here, so they fall through to "" (normal priority). HTML is stripped so
// the cue/question match runs on plain text.
func mentionText(m *dgraphStruct.DgraphMentions) string {
	var raw string
	switch {
	case m.Chat != nil && m.Chat.Body != "":
		raw = m.Chat.Body
	case m.Post != nil && m.Post.Text != "":
		raw = m.Post.Text
	case m.Comment != nil && m.Comment.Text != "":
		raw = m.Comment.Text
	default:
		return ""
	}
	return helpers.HTMLToPlainText(raw)
}

// isActionable reports whether text contains a question or an action cue —
// the signal that the reader is expected to DO or ANSWER something.
func isActionable(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	if strings.Contains(t, "?") {
		return true
	}
	for _, cue := range actionCues {
		if strings.Contains(t, cue) {
			return true
		}
	}
	return false
}

// activityScope returns the (kind, id) the item lives in: ("channel", chUUID)
// or ("chat", groupingID). Empty when the scope isn't resolvable from the
// loaded item (task/doc mentions carry no last-seen scope here).
func activityScope(item *dgraphModels.UnifiedActivityItem) (kind, id string) {
	switch item.ActivityType {
	case "MENTION":
		if item.Mention == nil {
			return "", ""
		}
		if item.Mention.Post != nil && item.Mention.Post.Channel != nil && item.Mention.Post.Channel.Uuid != "" {
			return "channel", item.Mention.Post.Channel.Uuid
		}
		if item.Mention.Chat != nil && item.Mention.Chat.DM != nil && item.Mention.Chat.DM.GroupingId != "" {
			return "chat", item.Mention.Chat.DM.GroupingId
		}
	case "COMMENT":
		if item.Comment == nil {
			return "", ""
		}
		if item.Comment.Post != nil && item.Comment.Post.Channel != nil && item.Comment.Post.Channel.Uuid != "" {
			return "channel", item.Comment.Post.Channel.Uuid
		}
		if item.Comment.Chat != nil && item.Comment.Chat.DM != nil && item.Comment.Chat.DM.GroupingId != "" {
			return "chat", item.Comment.Chat.DM.GroupingId
		}
		if item.Comment.ChatGroupingId != "" {
			return "chat", item.Comment.ChatGroupingId
		}
	}
	return "", ""
}

// parseActivityTime parses the RFC3339 timestamp stored on an activity item.
// Returns the zero time on any parse failure (caller skips demotion).
func parseActivityTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// demoteAnswered drops what asked the person something to low once they have
// answered it, so the Priority view is "what still needs me", not "what ever
// needed me": an approval request once it no longer waits on them (decided,
// withdrawn, or their part answered), a mention or a comment once they wrote
// in its conversation or replied in its thread after it. Opening the
// conversation isn't an answer: the Inbox's dots and badge go by that
// (looks.engaged), Priority doesn't. Only positive evidence demotes; nothing
// is hidden, the full feed lists every item still.
func demoteAnswered(items []dgraphModels.UnifiedActivityItem, a answers) {
	for i := range items {
		if items[i].Priority == PriorityHigh && a.answered(&items[i]) {
			items[i].Priority = PriorityLow
		}
	}
}

// answered reports whether the person has answered item, by a.
func (a answers) answered(item *dgraphModels.UnifiedActivityItem) bool {
	if id := approvalOf(item); id != "" {
		return a.settled[id]
	}
	at := parseActivityTime(item.Time)
	if at.IsZero() {
		return false
	}
	since := func(wrote map[string]time.Time, key string) bool {
		t, ok := wrote[key]
		return key != "" && ok && t.After(at)
	}
	kind, id := activityScope(item)
	if (kind == "channel" && since(a.channels, id)) || (kind == "chat" && since(a.chats, id)) {
		return true
	}
	_, thread := activityThread(item)
	return since(a.threads, thread)
}

// approvalOf is the request an approval-request notice asks the person to
// answer, by its id; "" for any other item, and for a request on something
// they may no longer open (they can't answer it there).
func approvalOf(item *dgraphModels.UnifiedActivityItem) string {
	if item.Notice == nil || item.Notice.Kind != approvalAsked || !item.Notice.CanOpen {
		return ""
	}
	return item.Notice.Data["approval_id"]
}

// activityThread is the thread a mention or a comment lives in, by the
// message it hangs off: ("post", uuid) for a channel message's, ("chat",
// uuid) for a DM or group message's. A mention in a message is in that
// message's thread, one in a reply in its parent's; empty when the loaded
// item says neither (task and doc mentions).
func activityThread(item *dgraphModels.UnifiedActivityItem) (kind, id string) {
	var c *dgraphStruct.DgraphComment
	switch {
	case item.ActivityType == "MENTION" && item.Mention != nil:
		if item.Mention.Post != nil && item.Mention.Post.Uuid != "" {
			return "post", item.Mention.Post.Uuid
		}
		if item.Mention.Chat != nil && item.Mention.Chat.Uuid != "" {
			return "chat", item.Mention.Chat.Uuid
		}
		c = item.Mention.Comment
	case item.ActivityType == "COMMENT":
		c = item.Comment
	}
	if c == nil {
		return "", ""
	}
	if c.Post != nil && c.Post.Uuid != "" {
		return "post", c.Post.Uuid
	}
	if c.Chat != nil && c.Chat.Uuid != "" {
		return "chat", c.Chat.Uuid
	}
	return "", ""
}
