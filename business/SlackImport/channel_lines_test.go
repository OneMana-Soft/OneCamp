package business

import (
	"fmt"
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A Slack channel's own events read as the changes OneCamp's lines say (chat
// B28): who did it, to whom, and what changed.
func TestSlacksChannelEventsAreTheChangesALineSays(t *testing.T) {
	for _, c := range []struct {
		name    string
		m       SlackMessage
		kind    resourceEventBusiness.Kind
		actor   string
		subject string
		data    map[string]string
	}{
		{"a join", SlackMessage{Subtype: "channel_join", User: "U2"}, resourceEventBusiness.MemberAdded, "U2", "U2", nil},
		{"an add", SlackMessage{Subtype: "channel_join", User: "U2", Inviter: "U1"}, resourceEventBusiness.MemberAdded, "U1", "U2", nil},
		{"a private channel's join", SlackMessage{Subtype: "group_join", User: "U2"}, resourceEventBusiness.MemberAdded, "U2", "U2", nil},
		{"a leave", SlackMessage{Subtype: "channel_leave", User: "U2"}, resourceEventBusiness.MemberRemoved, "U2", "U2", nil},
		{"a topic", SlackMessage{Subtype: "channel_topic", User: "U1", Topic: " Ship on Friday "}, resourceEventBusiness.ChannelTopicChanged, "U1", "", map[string]string{"topic": "Ship on Friday"}},
		{"a rename", SlackMessage{Subtype: "channel_name", User: "U1", Name: "launch", OldName: "q4-launch"}, resourceEventBusiness.ChannelRenamed, "U1", "", map[string]string{"name": "launch", "was": "q4-launch"}},
		{"archived", SlackMessage{Subtype: "channel_archive", User: "U1"}, resourceEventBusiness.ChannelArchived, "U1", "", nil},
		{"restored", SlackMessage{Subtype: "channel_unarchive", User: "U1"}, resourceEventBusiness.ChannelRestored, "U1", "", nil},
	} {
		if !isChannelLine(c.m.Subtype) {
			t.Errorf("%s: %s isn't taken as a line", c.name, c.m.Subtype)
		}
		kind, actor, subject, data, ok := channelLineEvent(&c.m)
		if !ok || kind != c.kind || actor != c.actor || subject != c.subject || fmt.Sprint(data) != fmt.Sprint(c.data) {
			t.Errorf("%s: %q %q %q %v ok=%v", c.name, kind, actor, subject, data, ok)
		}
	}
	// A rename with no new name says nothing; other system events aren't lines.
	if _, _, _, _, ok := channelLineEvent(&SlackMessage{Subtype: "channel_name", User: "U1"}); ok {
		t.Error("a rename with no name became a line")
	}
	for _, s := range []string{"channel_purpose", "reminder_add", "bot_add", "pinned_item", ""} {
		if isChannelLine(s) {
			t.Errorf("%q became a line", s)
		}
	}
}

// Slack's system notices come across only as a channel's lines: its history
// follows the operator's choice, and the notices no line says never come, in a
// channel or a conversation, whatever the option.
func TestSlacksSystemNoticesComeAcrossOnlyAsAChannelsLines(t *testing.T) {
	for _, c := range []struct {
		subtype         string
		skip, linesHere bool
		out             bool
	}{
		{"channel_join", false, true, false},
		{"group_topic", false, true, false},
		{"channel_join", true, true, true},
		{"group_topic", true, true, true},
		{"channel_join", false, false, true},
		{"channel_purpose", false, true, true},
		{"pinned_item", false, true, true},
		{"reminder_add", false, true, true},
		{"bot_add", false, false, true},
		{"", false, true, false},
		{"", true, false, false},
		{"bot_message", false, true, false},
		{"thread_broadcast", true, true, false},
	} {
		if got := leftOut(c.subtype, c.skip, c.linesHere); got != c.out {
			t.Errorf("%q (skip %v, lines here %v): left out %v, want %v", c.subtype, c.skip, c.linesHere, got, c.out)
		}
	}
}
