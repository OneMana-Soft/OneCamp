package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The durable pipeline's switch (chat A2): a surface goes the durable way only
// when MESSAGE_PIPELINE names it; everything else is today's write.

type recordingPipeline struct{ writes []Write }

func (r *recordingPipeline) Write(_ context.Context, _ *userModels.UserInfo, w Write) (Written, error) {
	r.writes = append(r.writes, w)
	return Written{ID: "today"}, nil
}

func TestTheSwitchNamesSurfacesByName(t *testing.T) {
	got := surfacesOf(" channel, DM ,,group ")
	if len(got) != 3 || !got["channel"] || !got["dm"] || !got["group"] {
		t.Errorf("surfaces %v", got)
	}
	if len(surfacesOf("")) != 0 {
		t.Error("an empty switch turns something on")
	}
}

func TestAChannelSendGoesTheDurableWayOnlyWhenSwitchedOn(t *testing.T) {
	prevOn, prevWrite := surfaceOn, writeChannel
	t.Cleanup(func() { surfaceOn, writeChannel = prevOn, prevWrite })
	durableWrites := 0
	writeChannel = func(context.Context, *userModels.UserInfo, Write) (Written, error) {
		durableWrites++
		return Written{ID: "durable"}, nil
	}
	today := &recordingPipeline{}
	p := durable{today: today}
	send := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "c1"}}
	dm := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: "a b"}}
	edit := Write{Op: OpEdit, To: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "c1"}}

	surfaceOn = func(string) bool { return false }
	if w, _ := p.Write(context.Background(), nil, send); w.ID != "today" || durableWrites != 0 {
		t.Fatal("a channel send went the durable way with the switch off")
	}

	surfaceOn = func(s string) bool { return s == SurfaceChannel }
	if w, _ := p.Write(context.Background(), nil, send); w.ID != "durable" || durableWrites != 1 {
		t.Fatal("a channel send didn't go the durable way with channels switched on")
	}
	// Only sends to the surfaces switched on: a DM, with channels alone on, and
	// an edit are today's.
	for _, w := range []Write{dm, edit} {
		if got, _ := p.Write(context.Background(), nil, w); got.ID != "today" {
			t.Errorf("%s to %s went the durable way", w.Op, w.To.Kind)
		}
	}
}

// A DM or a group send goes the durable way only when its own surface is
// switched on, each apart from the other and from channels.
func TestADMOrGroupSendGoesTheDurableWayOnlyWhenItsSurfaceIsOn(t *testing.T) {
	prevOn, prevDM, prevGroup := surfaceOn, writeDM, writeGroup
	t.Cleanup(func() { surfaceOn, writeDM, writeGroup = prevOn, prevDM, prevGroup })
	writeDM = func(context.Context, *userModels.UserInfo, Write) (Written, error) {
		return Written{ID: "durable dm"}, nil
	}
	writeGroup = func(context.Context, *userModels.UserInfo, Write) (Written, error) {
		return Written{ID: "durable group"}, nil
	}
	p := durable{today: &recordingPipeline{}}
	dm := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: "a b"}}
	group := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: "g"}}
	for _, c := range []struct {
		on        []string
		dm, group string
	}{
		{nil, "today", "today"},
		{[]string{SurfaceChannel}, "today", "today"},
		{[]string{SurfaceDM}, "durable dm", "today"},
		{[]string{SurfaceGroup}, "today", "durable group"},
		{[]string{SurfaceDM, SurfaceGroup}, "durable dm", "durable group"},
	} {
		on := map[string]bool{}
		for _, s := range c.on {
			on[s] = true
		}
		surfaceOn = func(s string) bool { return on[s] }
		if w, _ := p.Write(context.Background(), nil, dm); w.ID != c.dm {
			t.Errorf("with %v on, a DM was written %q", c.on, w.ID)
		}
		if w, _ := p.Write(context.Background(), nil, group); w.ID != c.group {
			t.Errorf("with %v on, a group message was written %q", c.on, w.ID)
		}
	}
}

func TestAHeldMessageIsAnsweredByItsKind(t *testing.T) {
	prev, prevChat := postWritten, chatStored
	t.Cleanup(func() { postWritten, chatStored = prev, prevChat })
	asked := ""
	postWritten = func(_ context.Context, id string) (bool, error) {
		asked = id
		if id == "broken" {
			return false, errors.New("graph down")
		}
		return id == "p-written", nil
	}
	chatStored = func(_ context.Context, id string) (bool, error) {
		asked = "chat " + id
		return id == "m-written", nil
	}
	ev := func(kind, message string) outboxBusiness.Event {
		return outboxBusiness.Event{Entities: map[string]string{"kind": kind, "message": message}}
	}
	if ok, err := resolveMessage(context.Background(), ev("channel", "p-written")); !ok || err != nil || asked != "p-written" {
		t.Errorf("a written post: %v %v", ok, err)
	}
	if ok, _ := resolveMessage(context.Background(), ev("channel", "p-lost")); ok {
		t.Error("a post whose node was never written was released")
	}
	if _, err := resolveMessage(context.Background(), ev("channel", "broken")); err == nil {
		t.Error("an unanswered question was taken as an answer")
	}
	for _, kind := range []string{"dm", "group"} {
		if ok, err := resolveMessage(context.Background(), ev(kind, "m-written")); !ok || err != nil || asked != "chat m-written" {
			t.Errorf("a written %s message: %v %v (asked %q)", kind, ok, err, asked)
		}
		if ok, _ := resolveMessage(context.Background(), ev(kind, "m-lost")); ok {
			t.Errorf("a %s message whose node was never written was released", kind)
		}
	}
	if ok, err := resolveMessage(context.Background(), ev("thread", "r1")); !ok || err != nil {
		t.Error("a kind with no answer of its own should be released: its change was committed with it")
	}
}

func TestTheSystemCheckSaysWhichSurfacesAreOn(t *testing.T) {
	if got := switchNote(""); !strings.Contains(got, "Every message is written as before") {
		t.Errorf("off: %q", got)
	}
	if got := switchNote("group,channel"); got != "These surfaces write through the outbox: channel, group." {
		t.Errorf("on: %q", got)
	}
}

// Channel posts and DMs and group messages are on one outbox topic, so one
// resolver answers for all of them and the drain treats them alike.
func TestPostsAndChatsShareOneMessageTopic(t *testing.T) {
	if postBusiness.MessageTopic != chatBusiness.MessageTopic || postBusiness.MessageCreated != chatBusiness.MessageCreated {
		t.Errorf("posts on %q (%q), chats on %q (%q)", postBusiness.MessageTopic, postBusiness.MessageCreated, chatBusiness.MessageTopic, chatBusiness.MessageCreated)
	}
}
