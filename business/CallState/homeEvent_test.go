package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// The ONE conversation an event's call's recap goes to (meet-02, main's ruling
// 4 (a)): two people's DM, three or more people's group, the same people the
// same group whatever their order or repeats. One person alone has none.
func TestAnEventsPeopleHaveOneConversation(t *testing.T) {
	a := uuid.MustParse("4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33")
	b := uuid.MustParse("5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44")
	c := uuid.MustParse("6b2e4c3a-5d9f-4e77-8c73-2f0d3b8c4e55")

	for _, alone := range [][]uuid.UUID{nil, {a}, {a, a}, {uuid.Nil, a}} {
		if ref, ok := ConversationOf(alone); ok {
			t.Errorf("ConversationOf(%v) = %+v, want none", alone, ref)
		}
	}

	dm, ok := ConversationOf([]uuid.UUID{a, b})
	if want := helpers.GetGroupingId(a.String(), b.String()); !ok || dm.Kind != "dm" || dm.ID != want {
		t.Errorf("two people = %+v %v, want their DM %q", dm, ok, want)
	}
	if again, ok := ConversationOf([]uuid.UUID{b, a, b}); !ok || again != dm {
		t.Errorf("the same two people, reordered and repeated = %+v %v, want %+v", again, ok, dm)
	}

	want, err := helpers.GenerateGroupID([]string{a.String(), b.String(), c.String()})
	if err != nil {
		t.Fatal(err)
	}
	group, ok := ConversationOf([]uuid.UUID{a, b, c})
	if !ok || group.Kind != "group" || group.ID != want {
		t.Errorf("three people = %+v %v, want their group %q", group, ok, want)
	}
	if again, ok := ConversationOf([]uuid.UUID{c, a, b, a}); !ok || again != group {
		t.Errorf("the same three people, reordered and repeated = %+v %v, want %+v", again, ok, group)
	}
}

// business/LiveKit keeps an event's call's transcript on the event, and asks
// here which event a room's call is (HomeOf): an event meeting's room and its
// breakout rooms answer the event; an instant meeting, a conversation and a
// name no call uses answer none, and none of them is an error.
func TestARoomsEventIsItsHomesEvent(t *testing.T) {
	eventMeeting := "6f1c0e2a-1111-4222-8333-944455556666"
	instant := "7a2d1f3b-2222-4333-8444-a55566667777"
	event := uuid.MustParse("9c4f3b5d-4444-4555-8666-c77788889999")
	defer func(was func(context.Context, string) (*model.Meeting, error)) { meetingOf = was }(meetingOf)
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		switch id {
		case eventMeeting:
			return &model.Meeting{ID: id, Kind: model.KindEvent, Event: event}, nil
		case instant:
			return &model.Meeting{ID: id, Kind: "instant"}, nil
		}
		return nil, nil
	}
	for room, want := range map[string]string{
		"meet-" + eventMeeting:                 event.String(),
		BreakoutRoom("meet-"+eventMeeting, 1):  event.String(),
		"meet-" + instant:                      "",
		"4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33": "",
		"not a call":                           "",
	} {
		got, err := eventOfRoom(context.Background(), room)
		if err != nil || got != want {
			t.Errorf("eventOfRoom(%q) = %q, %v; want %q", room, got, err, want)
		}
	}
}
