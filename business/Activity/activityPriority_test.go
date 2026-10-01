package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

func mentionItem(body string) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{
		ActivityType: "MENTION",
		Mention: &dgraphStruct.DgraphMentions{
			Chat: &dgraphStruct.DgraphChat{Body: body},
		},
	}
}

func TestScoreActivityPriority_Mentions(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"can you review this PR?", PriorityHigh},          // question + cue
		{"@me what do you think", PriorityNormal},          // no ? and no cue word ("what" alone isn't a cue)
		{"need your approval on the budget", PriorityHigh}, // action cue
		{"this is blocked on you", PriorityHigh},           // cue "blocked"
		{"thanks for the help earlier", PriorityHigh},      // cue "help" — acceptable over-trigger
		{"just fyi, shipped the build", PriorityNormal},    // informational
		{"", PriorityNormal},                               // empty body → still a direct mention
	}
	for _, c := range cases {
		got := scoreActivityPriority(mentionItem(c.body))
		if got != c.want {
			t.Errorf("mention %q: got %q want %q", c.body, got, c.want)
		}
	}
}

func TestScoreActivityPriority_QuestionMark(t *testing.T) {
	if got := scoreActivityPriority(mentionItem("are we still on for 3pm?")); got != PriorityHigh {
		t.Errorf("a question should be high priority, got %q", got)
	}
}

func TestScoreActivityPriority_Comments(t *testing.T) {
	actionable := dgraphModels.UnifiedActivityItem{
		ActivityType: "COMMENT",
		Comment:      &dgraphStruct.DgraphComment{Text: "could you take a look?"},
	}
	if got := scoreActivityPriority(actionable); got != PriorityHigh {
		t.Errorf("actionable comment should be high, got %q", got)
	}

	plain := dgraphModels.UnifiedActivityItem{
		ActivityType: "COMMENT",
		Comment:      &dgraphStruct.DgraphComment{Text: "looks good to me"},
	}
	if got := scoreActivityPriority(plain); got != PriorityNormal {
		t.Errorf("plain comment should be normal, got %q", got)
	}
}

func TestScoreActivityPriority_Reactions(t *testing.T) {
	r := dgraphModels.UnifiedActivityItem{ActivityType: "REACTION"}
	if got := scoreActivityPriority(r); got != PriorityLow {
		t.Errorf("reaction should be low, got %q", got)
	}
}

func TestScoreActivityPriority_HTMLStripped(t *testing.T) {
	// HTML tags around a question must not defeat the "?" detection.
	got := scoreActivityPriority(mentionItem("<p>can you confirm?</p>"))
	if got != PriorityHigh {
		t.Errorf("HTML-wrapped question should be high, got %q", got)
	}
}

func TestIsActionable(t *testing.T) {
	if !isActionable("WHO is handling this? ") {
		t.Error("question mark should be actionable (case-insensitive)")
	}
	if isActionable("   ") {
		t.Error("blank should not be actionable")
	}
	if !isActionable("Please APPROVE") {
		t.Error("cue should match case-insensitively")
	}
}

// channelMentionAt builds a high-priority channel mention at a given time.
func channelMentionAt(chUUID string, at time.Time) dgraphModels.UnifiedActivityItem {
	item := dgraphModels.UnifiedActivityItem{
		ActivityType: "MENTION",
		Time:         at.Format(time.RFC3339),
		Mention: &dgraphStruct.DgraphMentions{
			Post: &dgraphStruct.DgraphPost{
				Text:    "can you review this?",
				Channel: &dgraphStruct.DgraphChannel{Uuid: chUUID},
			},
		},
	}
	item.Priority = scoreActivityPriority(item)
	return item
}

func TestDemoteSeenActivities_DemotesWhenSeenAfter(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	items := []dgraphModels.UnifiedActivityItem{
		channelMentionAt("ch-seen", base),   // will be demoted
		channelMentionAt("ch-unseen", base), // stays high
	}
	if items[0].Priority != PriorityHigh || items[1].Priority != PriorityHigh {
		t.Fatalf("precondition: both should start high, got %q %q", items[0].Priority, items[1].Priority)
	}

	lastSeenChannels := map[string]time.Time{
		"ch-seen": base.Add(1 * time.Minute), // opened the channel AFTER the mention
		// ch-unseen has no row → never opened
	}
	demoteSeenActivities(items, lastSeenChannels, map[string]time.Time{})

	if items[0].Priority != PriorityLow {
		t.Errorf("a mention in a channel seen after the mention should be demoted, got %q", items[0].Priority)
	}
	if items[1].Priority != PriorityHigh {
		t.Errorf("a mention in an unseen channel should stay high, got %q", items[1].Priority)
	}
}

func TestDemoteSeenActivities_KeepsWhenSeenBefore(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	items := []dgraphModels.UnifiedActivityItem{channelMentionAt("ch-1", base)}

	// Last seen BEFORE the mention → not yet acknowledged → stays high.
	demoteSeenActivities(items, map[string]time.Time{"ch-1": base.Add(-1 * time.Hour)}, map[string]time.Time{})
	if items[0].Priority != PriorityHigh {
		t.Errorf("seen-before should keep high priority, got %q", items[0].Priority)
	}
}

func TestDemoteSeenActivities_UnresolvableScopeUntouched(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// A task mention carries no channel/grouping scope here.
	item := dgraphModels.UnifiedActivityItem{
		ActivityType: "MENTION",
		Time:         base.Format(time.RFC3339),
		Mention: &dgraphStruct.DgraphMentions{
			Task: &dgraphStruct.DgraphTask{Uuid: "t-1"},
		},
	}
	item.Priority = PriorityNormal
	items := []dgraphModels.UnifiedActivityItem{item}
	demoteSeenActivities(items, map[string]time.Time{}, map[string]time.Time{})
	if items[0].Priority != PriorityNormal {
		t.Errorf("unresolvable-scope item should be untouched, got %q", items[0].Priority)
	}
}

func TestActivityScope(t *testing.T) {
	chItem := &dgraphModels.UnifiedActivityItem{
		ActivityType: "MENTION",
		Mention: &dgraphStruct.DgraphMentions{
			Post: &dgraphStruct.DgraphPost{Channel: &dgraphStruct.DgraphChannel{Uuid: "ch-9"}},
		},
	}
	if k, id := activityScope(chItem); k != "channel" || id != "ch-9" {
		t.Errorf("expected channel/ch-9, got %s/%s", k, id)
	}

	chatComment := &dgraphModels.UnifiedActivityItem{
		ActivityType: "COMMENT",
		Comment:      &dgraphStruct.DgraphComment{ChatGroupingId: "grp-7"},
	}
	if k, id := activityScope(chatComment); k != "chat" || id != "grp-7" {
		t.Errorf("expected chat/grp-7, got %s/%s", k, id)
	}
}

func TestHasDemotableActivity(t *testing.T) {
	// Only reactions (already low) → nothing to demote → skip the reads.
	onlyReactions := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "REACTION", Priority: PriorityLow},
		{ActivityType: "REACTION", Priority: PriorityLow},
	}
	if hasDemotableActivity(onlyReactions) {
		t.Error("a reaction-only feed should not be demotable")
	}

	// A high item present → demotable.
	withHigh := append(onlyReactions, dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Priority: PriorityHigh})
	if !hasDemotableActivity(withHigh) {
		t.Error("a feed with a high item should be demotable")
	}

	// A normal item present → demotable.
	withNormal := []dgraphModels.UnifiedActivityItem{{ActivityType: "COMMENT", Priority: PriorityNormal}}
	if !hasDemotableActivity(withNormal) {
		t.Error("a feed with a normal item should be demotable")
	}

	// Empty feed → nothing to demote.
	if hasDemotableActivity(nil) {
		t.Error("empty feed should not be demotable")
	}
}
