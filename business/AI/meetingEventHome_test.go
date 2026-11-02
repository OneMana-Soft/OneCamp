package business

import (
	"context"
	"strings"
	"testing"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// main's ruling 4 (a): an event's call's recap goes to ONE conversation of the
// event's people (its organiser and the members it invites): two of them are
// their DM, three or more their group, the same people the same group. The
// organiser alone with guests by email has none; a member who has left the
// workspace is left out; an instant meeting keeps nothing; a conversation's
// call keeps its own conversation.
func TestAnEventsRecapGoesToItsPeoplesConversation(t *testing.T) {
	org, maya, lee, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	event, instant := uuid.New(), "meet-"+uuid.NewString()
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	keepHome, keepPeople, keepMembers := homeOf, eventCallPeople, activeMembers
	t.Cleanup(func() { homeOf, eventCallPeople, activeMembers = keepHome, keepPeople, keepMembers })
	homeOf = func(_ context.Context, room string) (callStateBusiness.Home, error) {
		switch room {
		case channel:
			return callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: "channel", ID: channel}}, nil
		case instant:
			return callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: "meeting", ID: strings.TrimPrefix(room, "meet-")}}, nil
		}
		return callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: "meeting", ID: strings.TrimPrefix(room, "meet-")}, Event: event}, nil
	}
	var people []uuid.UUID
	eventCallPeople = func(_ context.Context, id uuid.UUID) (*calendarBusiness.CallPeople, error) {
		if id != event {
			t.Errorf("the people of event %s, want %s", id, event)
		}
		return &calendarBusiness.CallPeople{Title: "Design review", Members: people}, nil
	}
	reads := 0
	activeMembers = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		reads++
		var out []*dgraphStruct.DgraphUser
		for i := len(ids) - 1; i >= 0; i-- { // in no particular order, as the store answers
			if ids[i] != gone.String() { // whoever left the workspace isn't in the answer
				out = append(out, &dgraphStruct.DgraphUser{Uid: "0x" + ids[i][:4], Uuid: ids[i]})
			}
		}
		return out, nil
	}

	at := func(room string) (recapHome, bool) {
		t.Helper()
		h, keeps, err := recapHomeOf(context.Background(), room)
		if err != nil {
			t.Fatalf("recapHomeOf(%q): %v", room, err)
		}
		return h, keeps
	}
	if h, keeps := at(channel); !keeps || h.conv.Kind != "channel" || h.conv.ID != channel || h.event != nil {
		t.Errorf("a channel's call = %+v %v, want its channel", h, keeps)
	}
	if _, keeps := at(instant); keeps {
		t.Error("an instant meeting's call keeps its recap somewhere")
	}

	room := "meet-" + uuid.NewString()
	people = []uuid.UUID{org, maya, gone}
	h, keeps := at(room)
	if want := helpers.GetGroupingId(org.String(), maya.String()); !keeps || h.conv.Kind != "dm" || h.conv.ID != want || h.event == nil || h.event.Title != "Design review" {
		t.Errorf("the organiser and one member (one more who left) = %+v %v, want their DM %q", h, keeps, want)
	}
	people = []uuid.UUID{org, maya, lee}
	h, _ = at(room)
	want, _ := helpers.GenerateGroupID([]string{org.String(), maya.String(), lee.String()})
	if h.conv.Kind != "group" || h.conv.ID != want || len(h.members) != 3 {
		t.Errorf("the organiser and two members = %+v, want their group %q of three", h, want)
	}
	if h.members[0].Uuid != org.String() {
		t.Errorf("the members start with %s, want the organiser first", h.members[0].Uuid)
	}
	people = []uuid.UUID{org}
	if h, keeps := at(room); !keeps || h.conv.Kind != "" || h.event == nil {
		t.Errorf("the organiser alone = %+v %v, want the event's artefacts with no conversation", h, keeps)
	}
	if reads != 3 {
		t.Errorf("%d reads of the members for 3 event calls, want one each", reads)
	}
}

// meet-02: an event's call's notes go to its people (the organiser and the
// members it invites, as the calendar has them now), including those who
// missed the call. A speaker or attendee who isn't one of them (someone let in
// by the call's link, or taken off the event since) is left out; a guest by
// email is never a member. No per-person store read: the people were read once.
func TestAnEventsNotesReachItsPeople(t *testing.T) {
	keep := mayOpenConversation
	t.Cleanup(func() { mayOpenConversation = keep })
	mayOpenConversation = func(context.Context, string, string) bool {
		t.Error("an event's call's notes asked a conversation")
		return true
	}
	h := recapHome{
		home:    callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: "meeting", ID: "6f1c0e2a-1111-4222-8333-944455556666"}, Event: uuid.New()},
		event:   &calendarBusiness.CallPeople{Title: "Design review"},
		members: []*dgraphStruct.DgraphUser{{Uid: "0x1", Uuid: "a"}, {Uid: "0x7", Uuid: "b"}},
	}
	call := liveKitBusiness.FinishedCall{Room: "meet-6f1c0e2a-1111-4222-8333-944455556666", SessionKey: "call-RM_abc"}
	got := notesAudience(context.Background(), call, h, []string{"0x1", "0x5", "0x9"})
	if want := "0x1,0x7"; strings.Join(got, ",") != want {
		t.Errorf("audience = %v, want %s", got, want)
	}
}
