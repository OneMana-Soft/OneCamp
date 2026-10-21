package business

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// What each channel change says in the channel's history (B28, SD-34 to 36).
func TestAChannelChangeReadsAsALine(t *testing.T) {
	maya := &dgraphStruct.DgraphUser{Uuid: "u-maya", UserName: "Maya"}
	priya := &dgraphStruct.DgraphUser{Uuid: "u-priya", UserName: "Priya"}
	jonas := &dgraphStruct.DgraphUser{Uuid: "u-jonas", UserName: "Jonas"}
	people := map[string]*dgraphStruct.DgraphUser{"u-maya": maya, "u-priya": priya, "u-jonas": jonas}
	small := channelFacts{Members: 8}
	ev := func(kind resourceEventBusiness.Kind, actor string, subjects []string, data map[string]string) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: kind, ResourceKind: KindChannel, ResourceUUID: "c1",
			Actor: resourceEventBusiness.Actor{UserUUID: actor}, Subjects: subjects, Data: data}
	}
	for _, c := range []struct {
		name string
		ev   resourceEventBusiness.Event
		kind string
		text string
	}{
		{"a join", ev(resourceEventBusiness.MemberAdded, "u-maya", []string{"u-maya"}, nil), LineChannelJoined, "Maya joined."},
		{"an add", ev(resourceEventBusiness.MemberAdded, "u-maya", []string{"u-priya", "u-jonas"}, nil), LineChannelAdded, "Maya added Priya and Jonas."},
		{"a leave", ev(resourceEventBusiness.MemberRemoved, "u-jonas", []string{"u-jonas"}, nil), LineChannelLeft, "Jonas left."},
		{"a removal", ev(resourceEventBusiness.MemberRemoved, "u-maya", []string{"u-jonas"}, nil), LineChannelRemoved, "Maya removed Jonas."},
		{"a rename", ev(resourceEventBusiness.ChannelRenamed, "u-maya", nil, map[string]string{"name": "launch", "was": "q4-launch"}), LineChannelRenamed, "Maya renamed the channel to #launch (was #q4-launch)."},
		{"a topic", ev(resourceEventBusiness.ChannelTopicChanged, "u-jonas", nil, map[string]string{"topic": "Ship on Friday"}), LineChannelTopic, "Jonas set the topic: Ship on Friday"},
		{"a topic cleared", ev(resourceEventBusiness.ChannelTopicChanged, "u-jonas", nil, map[string]string{"topic": ""}), LineChannelTopic, "Jonas removed the topic."},
		{"made private", ev(resourceEventBusiness.ChannelPrivacyChanged, "u-maya", nil, map[string]string{"private": "true"}), LineChannelPrivate, "Maya made this channel private. Only its members can find it and read it now."},
		{"made public", ev(resourceEventBusiness.ChannelPrivacyChanged, "u-maya", nil, map[string]string{"private": "false"}), LineChannelPublic, "Maya made this channel public. Everyone in the workspace can now find it and read its history and files."},
		{"archived", ev(resourceEventBusiness.ChannelArchived, "u-maya", nil, nil), LineChannelArchived, "Maya archived the channel."},
		{"restored", ev(resourceEventBusiness.ChannelRestored, "u-maya", nil, nil), LineChannelRestored, "Maya restored the channel."},
	} {
		l, text, show := channelLine(c.ev, people, small, false)
		if !show || l.Kind != c.kind || text != c.text {
			t.Errorf("%s: %q %q shown=%v; want %q %q", c.name, l.Kind, text, show, c.kind, c.text)
		}
		if l.Actor != c.ev.Actor.UserUUID {
			t.Errorf("%s: actor %q", c.name, l.Actor)
		}
	}
}

// Joins and leaves show in private channels and channels of up to 20; a
// larger public channel shows them only where the workspace says so. Every
// other line always shows.
func TestJoinsAndLeavesShowInSmallOrPrivateChannels(t *testing.T) {
	people := map[string]*dgraphStruct.DgraphUser{"u": {Uuid: "u", UserName: "Sam"}}
	join := resourceEventBusiness.Event{Kind: resourceEventBusiness.MemberAdded, ResourceKind: KindChannel, ResourceUUID: "c",
		Actor: resourceEventBusiness.Actor{UserUUID: "u"}, Subjects: []string{"u"}}
	rename := resourceEventBusiness.Event{Kind: resourceEventBusiness.ChannelRenamed, ResourceKind: KindChannel, ResourceUUID: "c",
		Actor: resourceEventBusiness.Actor{UserUUID: "u"}, Data: map[string]string{"name": "a", "was": "b"}}
	for _, c := range []struct {
		facts   channelFacts
		large   bool
		joinsOK bool
	}{
		{channelFacts{Members: 20}, false, true},
		{channelFacts{Members: 21}, false, false},
		{channelFacts{Members: 400, Private: true}, false, true},
		{channelFacts{Members: 400}, true, true},
	} {
		if _, _, show := channelLine(join, people, c.facts, c.large); show != c.joinsOK {
			t.Errorf("%+v large=%v: join shown=%v", c.facts, c.large, show)
		}
		if _, _, show := channelLine(rename, people, c.facts, c.large); !show {
			t.Errorf("%+v: a rename was hidden", c.facts)
		}
	}
}
