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

// scoreActivityPriority assigns a coarse priority to one activity item. It is
// pure and side-effect free. Heuristics, in order of strength:
//   - MENTION of the user that asks a question or carries an action cue → high
//   - MENTION (plain) → normal
//   - COMMENT on the user's content that asks a question / action cue → high
//   - COMMENT (plain) → normal
//   - REACTION → low (nice-to-know, rarely actionable)
func scoreActivityPriority(item dgraphModels.UnifiedActivityItem) string {
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

// hasDemotableActivity reports whether any item could be demoted by
// read-state (i.e. is currently high or normal). When false — e.g. a feed
// of only reactions, which are already low — the caller can skip the
// last-seen reads entirely and pay zero extra queries.
func hasDemotableActivity(items []dgraphModels.UnifiedActivityItem) bool {
	for i := range items {
		if items[i].Priority == PriorityHigh || items[i].Priority == PriorityNormal {
			return true
		}
	}
	return false
}

// demoteSeenActivities downgrades the priority of items the user has ALREADY
// ENGAGED WITH, so the Priority tab reflects "what still needs me" rather
// than "what ever needed me". The signal is read-state: if the user's
// last-seen for an item's scope (channel / DM-or-group) is at or after the
// item's timestamp, they've opened that conversation since — which is the
// primary acknowledgment signal in a chat product. Such items are demoted
// to PriorityLow (they drop out of the high-priority Priority view) but are
// NEVER hidden from the full feed.
//
// Cheap by construction: the two last-seen maps are each a single batched
// query; this pass is O(items) with map lookups, no per-item round-trips.
// Items whose scope can't be resolved (e.g. task/doc mentions) are left
// as-is — we only demote when we have positive evidence of engagement.
func demoteSeenActivities(items []dgraphModels.UnifiedActivityItem, lastSeenChannels, lastSeenChats map[string]time.Time) {
	for i := range items {
		// Reactions are already low; nothing to demote.
		if items[i].Priority == "" || items[i].Priority == PriorityLow {
			continue
		}
		scopeKind, scopeID := activityScope(&items[i])
		if scopeKind == "" || scopeID == "" {
			continue
		}
		itemTime := parseActivityTime(items[i].Time)
		if itemTime.IsZero() {
			continue
		}
		var seen time.Time
		var ok bool
		switch scopeKind {
		case "channel":
			seen, ok = lastSeenChannels[scopeID]
		case "chat":
			seen, ok = lastSeenChats[scopeID]
		}
		// Seen AT OR AFTER the item → the user has opened this conversation
		// since the mention/comment; treat as acknowledged.
		if ok && !seen.Before(itemTime) {
			items[i].Priority = PriorityLow
		}
	}
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
