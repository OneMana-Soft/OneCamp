package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
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
	// Only sends to channels, so far: a DM and an edit are today's.
	for _, w := range []Write{dm, edit} {
		if got, _ := p.Write(context.Background(), nil, w); got.ID != "today" {
			t.Errorf("%s to %s went the durable way", w.Op, w.To.Kind)
		}
	}
}

func TestAHeldMessageIsAnsweredByItsKind(t *testing.T) {
	prev := postWritten
	t.Cleanup(func() { postWritten = prev })
	asked := ""
	postWritten = func(_ context.Context, id string) (bool, error) {
		asked = id
		if id == "broken" {
			return false, errors.New("graph down")
		}
		return id == "p-written", nil
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
	if ok, err := resolveMessage(context.Background(), ev("dm", "m1")); !ok || err != nil {
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
