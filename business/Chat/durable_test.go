package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The durable DM's and group message's steps (chat A2, durable.go): the rows
// and events, then the node, then a new conversation's setup, then the events
// released (the live one saying whom a new conversation is for) and
// delivered; a node that fails takes the rows back; an event that can't be
// released is left to its resolver; a send whose client id has a message
// already answers that message once its node is written.

type chatSteps struct {
	mu                   sync.Mutex
	rowsErr, nodeErr     error
	dmUID                string
	unreleased           int64
	firstThere           bool
	did, tookBack, asked []string
}

func (s *chatSteps) note(f string, a ...any) {
	s.mu.Lock()
	s.did = append(s.did, fmt.Sprintf(f, a...))
	s.mu.Unlock()
}

func recordChatSteps(t *testing.T, s *chatSteps) {
	t.Helper()
	pRows, pDM, pGroup, pStartDM, pStartGroup, pRead, pRelease, pDeliver, pTakeBack, pAnswered, pReadChat := writeChatRows, writeDM, writeGroup, startNewDM, startNewGroup, markRead, releaseEvent, deliverEvent, takeBackChatFn, chatAnswered, readChat
	t.Cleanup(func() {
		writeChatRows, writeDM, writeGroup, startNewDM, startNewGroup, markRead, releaseEvent, deliverEvent, takeBackChatFn, chatAnswered, readChat = pRows, pDM, pGroup, pStartDM, pStartGroup, pRead, pRelease, pDeliver, pTakeBack, pAnswered, pReadChat
	})
	writeChatRows = func(_ context.Context, kind, conversation string, _, _ uuid.UUID, clientID string, _ time.Time, _ string) ([]int64, error) {
		s.note("rows %s %s %s", kind, conversation, clientID)
		if s.rowsErr != nil {
			return nil, s.rowsErr
		}
		return []int64{7, 8}, nil
	}
	writeDM = func(context.Context, uuid.UUID, *adapter.ChatInfo, *userModels.UserInfo, *dgraphStruct.DgraphUser, string, []*dgraphStruct.DgraphUser, time.Time) (*dgraphStruct.DgraphChat, string, error) {
		s.note("node")
		return nil, s.dmUID, s.nodeErr
	}
	writeGroup = func(_ context.Context, _ uuid.UUID, _ *adapter.ChatInfo, _ *userModels.UserInfo, _ []*dgraphStruct.DgraphUser, newMembers []*dgraphStruct.DgraphUser, _ time.Time) (*dgraphStruct.DgraphChat, string, error) {
		s.note("node with %d new members", len(newMembers))
		return nil, s.dmUID, s.nodeErr
	}
	startNewDM = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphUser, uuid.UUID, string, time.Time) {
		s.note("start dm")
	}
	startNewGroup = func(_ context.Context, _ *userModels.UserInfo, _ string, people []string, _ time.Time) {
		s.note("start group of %d", len(people))
	}
	markRead = func(context.Context, string, string, *userModels.UserInfo, time.Time) { s.note("read") }
	releaseEvent = func(_ context.Context, id int64, more map[string]string) error {
		if more != nil {
			s.note("release %d opened %s", id, more["opened"])
		} else {
			s.note("release %d", id)
		}
		if id == s.unreleased {
			return errors.New("postgres away")
		}
		return nil
	}
	deliverEvent = func(id int64) { s.note("deliver %d", id) }
	takeBackChatFn = func(_ context.Context, chat uuid.UUID, events []int64) error {
		s.mu.Lock()
		s.tookBack = append(s.tookBack, fmt.Sprintf("%v", events))
		s.mu.Unlock()
		return nil
	}
	chatAnswered = func(_ context.Context, _ uuid.UUID, conversation, clientID string) (*adapter.OutputCreateChatForChat, error) {
		s.asked = append(s.asked, conversation+" "+clientID)
		return &adapter.OutputCreateChatForChat{Uuid: "the-first-message"}, nil
	}
	readChat = func(_ context.Context, id string) (*dgraphStruct.DgraphChat, error) {
		if !s.firstThere {
			return nil, domain.ErrChatGone
		}
		return &dgraphStruct.DgraphChat{Uuid: id}, nil
	}
}

func durableDM(t *testing.T) (*adapter.OutputCreateChatForChat, error) {
	t.Helper()
	ada, sam := person("8d5f8a8e-0000-4000-8000-00000000000a", "0xa", "ada", false), person("8d5f8a8e-0000-4000-8000-00000000000b", "0xb", "sam", false)
	return CreateDMDurably(context.Background(), &adapter.ChatInfo{ToUuid: sam.Uuid, TextHtml: "<p>hi</p>"}, asUser(ada), sam, uuid.MustParse(sam.Uuid), nil, "local-1")
}

func TestADurableDMIsReleasedAndDeliveredOnceItsNodeIsWritten(t *testing.T) {
	s := &chatSteps{}
	recordChatSteps(t, s)
	out, err := durableDM(t)
	if err != nil || out == nil || out.Uuid == "" {
		t.Fatalf("%+v %v", out, err)
	}
	grp := helpers.GetGroupingId("8d5f8a8e-0000-4000-8000-00000000000a", "8d5f8a8e-0000-4000-8000-00000000000b")
	if got := strings.Join(s.did, ", "); got != "rows dm "+grp+" local-1, node, read, release 7, deliver 7, release 8, deliver 8" {
		t.Errorf("steps: %s", got)
	}
}

func TestAFirstDurableDMSetsItsConversationUpAndSaysWhomItsFor(t *testing.T) {
	s := &chatSteps{dmUID: "0xdm"}
	recordChatSteps(t, s)
	if _, err := durableDM(t); err != nil {
		t.Fatal(err)
	}
	want := "node, start dm, read, release 7 opened 8d5f8a8e-0000-4000-8000-00000000000a,8d5f8a8e-0000-4000-8000-00000000000b, deliver 7, release 8, deliver 8"
	if got := strings.Join(s.did[1:], ", "); got != want {
		t.Errorf("steps:\n got  %s\n want %s", got, want)
	}
}

func TestANewGroupsDurableFirstMessageMakesItsIDFromItsPeople(t *testing.T) {
	s := &chatSteps{dmUID: "0xgrp"}
	recordChatSteps(t, s)
	ada, sam, jonas := person("8d5f8a8e-0000-4000-8000-00000000000a", "0xa", "ada", false), person("8d5f8a8e-0000-4000-8000-00000000000b", "0xb", "sam", false), person("8d5f8a8e-0000-4000-8000-00000000000c", "0xc", "jonas", false)
	in := &adapter.ChatInfo{TextHtml: "<p>welcome</p>"}
	if _, err := CreateGroupMessageDurably(context.Background(), in, asUser(ada), nil, []*dgraphStruct.DgraphUser{ada, sam, jonas}, "local-2"); err != nil {
		t.Fatal(err)
	}
	grp, _ := helpers.GenerateGroupID([]string{ada.Uuid, sam.Uuid, jonas.Uuid})
	if in.GrpUuid != grp {
		t.Errorf("the group's id is %q, want %q", in.GrpUuid, grp)
	}
	want := "rows group " + grp + " local-2, node with 3 new members, start group of 3, read, release 7 opened " + ada.Uuid + "," + sam.Uuid + "," + jonas.Uuid + ", deliver 7, release 8, deliver 8"
	if got := strings.Join(s.did, ", "); got != want {
		t.Errorf("steps:\n got  %s\n want %s", got, want)
	}
}

func TestADurableMessageWhoseNodeFailsIsTakenBack(t *testing.T) {
	s := &chatSteps{nodeErr: errors.New("graph down")}
	recordChatSteps(t, s)
	out, err := durableDM(t)
	if err == nil || out != nil {
		t.Fatalf("a message with no node answered %+v", out)
	}
	if got := strings.Join(s.did[1:], ", "); got != "node" {
		t.Errorf("steps after the rows: %s: nothing set up or released for a message with no node", got)
	}
	if strings.Join(s.tookBack, ";") != "[7 8]" {
		t.Errorf("took back %v: its rows go, and both its events", s.tookBack)
	}
}

func TestADurableMessagesEventItsWriterCouldntReleaseIsLeftToItsResolver(t *testing.T) {
	s := &chatSteps{unreleased: 7}
	recordChatSteps(t, s)
	if _, err := durableDM(t); err != nil {
		t.Fatalf("the message is written, so it answers: %v", err)
	}
	if got := strings.Join(s.did[1:], ", "); got != "node, read, release 7, release 8, deliver 8" {
		t.Errorf("steps: %s: a held event isn't delivered, and the other goes on", got)
	}
}

func TestADurableSendTriedAgainAnswersItsFirstTryOnceItsNodeIsThere(t *testing.T) {
	s := &chatSteps{rowsErr: errChatSentAlready, firstThere: true}
	recordChatSteps(t, s)
	out, err := durableDM(t)
	if err != nil || out == nil || out.Uuid != "the-first-message" {
		t.Fatalf("%+v %v: the first message's answer", out, err)
	}
	if len(s.did) != 1 || len(s.asked) != 1 || !strings.HasSuffix(s.asked[0], " local-1") {
		t.Errorf("did %v, asked %v: no second node, no events", s.did, s.asked)
	}

	// While its first try's message has no node, it is still being sent.
	s2 := &chatSteps{rowsErr: errChatSentAlready}
	recordChatSteps(t, s2)
	if out, err := durableDM(t); !errors.Is(err, ErrStillBeingSent) || out != nil {
		t.Errorf("%+v %v: still being sent", out, err)
	}
}

// A message's live event keeps its conversation's order and the rest keep its
// own; an imported one is only indexed.
func TestAMessagesLiveEventIsInItsConversationsOrderAndTheRestInItsOwn(t *testing.T) {
	chat := uuid.New()
	for _, kind := range []string{mqttStruct.CONVERSATION_KIND_DM, mqttStruct.CONVERSATION_KIND_GROUP} {
		events := chatEvents(kind, "grp-1", chat, "u-ada", false)
		if len(events) != 2 {
			t.Fatalf("%s: %d events", kind, len(events))
		}
		live, rest := events[0], events[1]
		if live.Key != kind+":grp-1" || strings.Join(live.Sinks, ",") != "chat.access,chat.realtime" {
			t.Errorf("%s live: %s %v", kind, live.Key, live.Sinks)
		}
		if rest.Key != "chat:"+chat.String() || strings.Join(rest.Sinks, ",") != "chat.search,chat.embed,chat.notify,chat.events" {
			t.Errorf("%s rest: %s %v", kind, rest.Key, rest.Sinks)
		}
		for _, e := range events {
			if e.Topic != MessageTopic || e.Kind != MessageCreated || e.Actor != "u-ada" || e.Entities["message"] != chat.String() || e.Entities["conversation"] != "grp-1" || e.Entities["kind"] != kind {
				t.Errorf("event %+v", e)
			}
		}
	}
	imported := chatEvents(mqttStruct.CONVERSATION_KIND_DM, "grp-1", chat, "u-ada", true)
	if len(imported) != 1 || imported[0].Key != "chat:"+chat.String() || strings.Join(imported[0].Sinks, ",") != "chat.search" {
		t.Errorf("an imported message's events: %+v", imported)
	}
}

func TestAChatClientIDOnceDoesntTakeIsntKept(t *testing.T) {
	for id, want := range map[string]any{
		"local-1":               "local-1",
		"":                      nil,
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): nil,
		"has space":             nil,
	} {
		if got := chatClientID(id); got != want {
			t.Errorf("chatClientID(%q) = %v, want %v", id, got, want)
		}
	}
}
