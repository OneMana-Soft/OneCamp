package business

import (
	"context"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// A run's moment is its triggerer's Inbox notice (C2): stored, so the Inbox
// lists it and the badge counts it, not a live item no list ever held. It
// names the place they asked in, carries the agent's words beside it, and is
// from the agent.
func TestARunsMomentIsAnInboxNotice(t *testing.T) {
	agent := &model.AiAgent{Name: "Release Captain"}
	bot := &userBusiness.BotIdentity{UUID: "bot-uuid", DgraphUID: "0xbot", Name: "Release Captain"}

	n := triggererNotice(agent, bot, Surface{Kind: SurfaceChannelPost, ChannelID: "ch1", PostID: "p1"}, NoticeAgentBlocked, "  I'm blocked:\n\nwhich region?  ")
	if n == nil {
		t.Fatal("a moment in a channel is no notice")
	}
	if n.Kind != NoticeAgentBlocked || n.ResourceKind != "channel" || n.ResourceUUID != "ch1" || n.Data["post_id"] != "p1" {
		t.Errorf("a moment in a channel is %+v; want an agent.blocked notice about channel ch1, at post p1", n)
	}
	if n.Data[notificationBusiness.NoticeWords] != "I'm blocked: which region?" {
		t.Errorf("its words are %q; want the agent's, on one line", n.Data[notificationBusiness.NoticeWords])
	}
	if n.Agent == nil || n.Agent.Uuid != "bot-uuid" || n.Actor != nil {
		t.Errorf("it is from %+v (actor %+v); want the agent", n.Agent, n.Actor)
	}
	if n.DedupKey == "" {
		t.Error("it has no dedup key, so a second moment would be dropped or the same one told twice")
	}

	g := triggererNotice(agent, bot, Surface{Kind: SurfaceGroupChat, GroupID: "g1", MessageID: "m1"}, NoticeAgentReplied, "Done")
	if g == nil || g.Kind != NoticeAgentReplied || g.ResourceKind != "conversation" || g.ResourceUUID != "g1" || g.Data["message_id"] != "m1" {
		t.Errorf("a moment in a group is %+v; want an agent.replied notice about conversation g1, at message m1", g)
	}
	if d := triggererNotice(agent, bot, Surface{Kind: SurfaceDM, GroupID: "d1", MessageID: "m2"}, NoticeAgentReplied, "Done"); d == nil || d.ResourceKind != "conversation" {
		t.Errorf("a moment in a DM is %+v; want a notice about the conversation", d)
	}
	if n2 := triggererNotice(agent, bot, Surface{Kind: SurfaceChannelPost, ChannelID: "ch1", PostID: "p1"}, NoticeAgentBlocked, "again"); n2 == nil || n2.DedupKey == n.DedupKey {
		t.Error("a second moment of the same run shares the first's dedup key, so it is never told")
	}
	for _, s := range []Surface{{Kind: SurfaceTask}, {Kind: SurfaceChannelPost, ChannelID: "ch1"}, {Kind: SurfaceGroupChat, MessageID: "m1"}} {
		if x := triggererNotice(agent, bot, s, NoticeAgentReplied, "Done"); x != nil {
			t.Errorf("%+v made a notice %+v; want none (the task surface has its own path; no place, no notice)", s, x)
		}
	}
}

// The moment goes to the triggerer alone, through the notices (Tell), in the
// Inbox only, and only while they can still see where they asked.
func TestARunsMomentIsToldOnlyToWhoCanStillSeeWhereTheyAsked(t *testing.T) {
	savedTell, savedReach, savedBot, savedNames := tellTriggerer, reach, agentBotOf, channelNames
	t.Cleanup(func() { tellTriggerer, reach, agentBotOf, channelNames = savedTell, savedReach, savedBot, savedNames })
	channelNames = func(_ context.Context, ids []string) (map[string]string, error) {
		return map[string]string{"ch1": "launch"}, nil
	}
	agentBotOf = func(context.Context, *model.AiAgent) *userBusiness.BotIdentity {
		return &userBusiness.BotIdentity{UUID: "bot-uuid", DgraphUID: "0xbot"}
	}
	reach = sponsorReach{
		readsChannel:   func(_ context.Context, user, id string) (bool, error) { return user == "ana" && id == "ch1", nil },
		inConversation: func(_ context.Context, user, id string) (bool, error) { return user == "ana" && id == "g1", nil },
	}
	type told struct {
		n  notificationBusiness.Notice
		to []notificationBusiness.NoticeRecipient
	}
	var got []told
	tellTriggerer = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		got = append(got, told{n, to})
		return nil
	}
	agent := &model.AiAgent{Name: "Release Captain"}
	inChannel := Surface{Kind: SurfaceChannelPost, ChannelID: "ch1", PostID: "p1"}
	inGroup := Surface{Kind: SurfaceGroupChat, GroupID: "g1", MessageID: "m1"}
	for _, who := range []string{"ana", "cy"} {
		notifyTriggerer(context.Background(), agent, inChannel, who, NoticeAgentReplied, "Done")
		notifyTriggerer(context.Background(), agent, inGroup, who, NoticeAgentBlocked, "Which region?")
	}
	if len(got) != 2 {
		t.Fatalf("%d notices were told; want ana's two, none for cy, who can't see where they asked", len(got))
	}
	for _, g := range got {
		if len(g.to) != 1 || g.to[0].UserUUID != "ana" || !g.to[0].InboxOnly {
			t.Errorf("%s was told to %+v; want ana alone, in the Inbox only", g.n.Kind, g.to)
		}
	}
	if got[0].n.ResourceKind != "channel" || got[0].n.ResourceTitle != "launch" || got[1].n.Kind != NoticeAgentBlocked {
		t.Errorf("told %s about %s %q, then %s; want the reply in #launch, then the group's block", got[0].n.Kind, got[0].n.ResourceKind, got[0].n.ResourceTitle, got[1].n.Kind)
	}
}
