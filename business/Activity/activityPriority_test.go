package business

import (
	"testing"

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

func TestScorePriority_Mentions(t *testing.T) {
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
		got := ScorePriority(mentionItem(c.body))
		if got != c.want {
			t.Errorf("mention %q: got %q want %q", c.body, got, c.want)
		}
	}
}

func TestScorePriority_QuestionMark(t *testing.T) {
	if got := ScorePriority(mentionItem("are we still on for 3pm?")); got != PriorityHigh {
		t.Errorf("a question should be high priority, got %q", got)
	}
}

func TestScorePriority_Comments(t *testing.T) {
	actionable := dgraphModels.UnifiedActivityItem{
		ActivityType: "COMMENT",
		Comment:      &dgraphStruct.DgraphComment{Text: "could you take a look?"},
	}
	if got := ScorePriority(actionable); got != PriorityHigh {
		t.Errorf("actionable comment should be high, got %q", got)
	}

	plain := dgraphModels.UnifiedActivityItem{
		ActivityType: "COMMENT",
		Comment:      &dgraphStruct.DgraphComment{Text: "looks good to me"},
	}
	if got := ScorePriority(plain); got != PriorityNormal {
		t.Errorf("plain comment should be normal, got %q", got)
	}
}

func TestScorePriority_Reactions(t *testing.T) {
	r := dgraphModels.UnifiedActivityItem{ActivityType: "REACTION"}
	if got := ScorePriority(r); got != PriorityLow {
		t.Errorf("reaction should be low, got %q", got)
	}
}

func TestScorePriority_HTMLStripped(t *testing.T) {
	// HTML tags around a question must not defeat the "?" detection.
	got := ScorePriority(mentionItem("<p>can you confirm?</p>"))
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

// The markers are read only for a page with an item in a conversation: a
// page of reactions and notices costs no read.
func TestInConversation(t *testing.T) {
	quiet := []dgraphModels.UnifiedActivityItem{
		{ActivityType: "REACTION", Priority: PriorityLow},
		{ActivityType: "NOTICE", Priority: PriorityNormal},
		{ActivityType: "MENTION", Priority: PriorityNormal, Mention: &dgraphStruct.DgraphMentions{Task: &dgraphStruct.DgraphTask{Uuid: "t-1"}}},
	}
	if inConversation(quiet) {
		t.Error("reactions, notices and a task's mention have no conversation to read")
	}
	if inConversation(nil) {
		t.Error("an empty page has no conversation")
	}
	withReply := append(quiet, dgraphModels.UnifiedActivityItem{ActivityType: "COMMENT", Comment: &dgraphStruct.DgraphComment{ChatGroupingId: "grp-1"}})
	if !inConversation(withReply) {
		t.Error("a reply in a conversation has its marker to read")
	}
}
