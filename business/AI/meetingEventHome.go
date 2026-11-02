package business

// Where a finished call's recap and notes go (meet-02). A conversation's call
// keeps them in its conversation. An event's call keeps them on the event and
// posts its recap in the ONE conversation of the event's people (main's ruling
// 4 (a)): its organiser and the members it invites, never a guest by email,
// never someone who has left the workspace. Two of them have their DM, three
// or more their group (the same people, the same group), made by the recap
// when it is the first artefact. The organiser alone with guests by email has
// no conversation: the recap's notes stay on the event. An instant meeting
// keeps nothing.

import (
	"context"
	"fmt"
	"slices"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// maxEventConversation is the most people an event's conversation holds. An
// event that invites more (an all-hands) keeps its recap's notes on the event
// only: a group that size is a channel's job.
const maxEventConversation = 50

// Seams for tests: the call's home, the event's people, a member by uuid.
var (
	homeOf          = callStateBusiness.HomeOf
	eventCallPeople = calendarBusiness.EventCallPeople
	activeMembers   = userDomain.GetActiveDgraphUsersByUUIDsLight
)

// recapHome is where a finished call's recap and notes go.
type recapHome struct {
	home callStateBusiness.Home
	// conv is where the recap is posted; Kind "" for nowhere.
	conv conversationBusiness.Ref
	// event is the event's title and people, for an event's call; members
	// are those of its people who are still members, organiser first.
	event   *calendarBusiness.CallPeople
	members []*dgraphStruct.DgraphUser
}

// recapHomeOf answers where the call in a room keeps its recap and notes, and
// false for a call that keeps none (an instant meeting).
func recapHomeOf(ctx context.Context, room string) (recapHome, bool, error) {
	home, err := homeOf(ctx, room)
	if err != nil {
		return recapHome{}, false, err
	}
	if home.Ref.Kind != callStateBusiness.KindMeeting {
		return recapHome{home: home, conv: home.Ref}, true, nil
	}
	if home.Event == uuid.Nil {
		return recapHome{}, false, nil
	}
	people, err := eventCallPeople(ctx, home.Event)
	if err != nil {
		return recapHome{}, false, fmt.Errorf("the event's people: %w", err)
	}
	h := recapHome{home: home, event: people}
	if len(people.Members) > maxEventConversation {
		return h, true, nil
	}
	asked := make([]string, 0, len(people.Members))
	for _, id := range people.Members {
		asked = append(asked, id.String())
	}
	// One read; whoever has left the workspace isn't in it. A guess at who is
	// still here would name a different group, so a failed read fails.
	found, err := activeMembers(ctx, asked)
	if err != nil {
		return recapHome{}, false, fmt.Errorf("the event's members: %w", err)
	}
	byUUID := make(map[string]*dgraphStruct.DgraphUser, len(found))
	for _, u := range found {
		if u != nil && u.Uid != "" && (u.DeletedAt == nil || !u.DeletedAt.After(time.Unix(0, 0))) {
			byUUID[u.Uuid] = u
		}
	}
	ids := make([]uuid.UUID, 0, len(people.Members))
	for _, id := range people.Members {
		if u := byUUID[id.String()]; u != nil {
			h.members = append(h.members, u)
			ids = append(ids, id)
		}
	}
	if conv, ok := callStateBusiness.ConversationOf(ids); ok {
		h.conv = conv
	}
	return h, true, nil
}

// surfaceName is the readable label of where the call happened, for its notes'
// title: the event's title, or the channel's name (meetingSurfaceName).
func (h recapHome) surfaceName(ctx context.Context, room, viewerUID string) string {
	if h.event != nil {
		return h.event.Title
	}
	return meetingSurfaceName(ctx, room, viewerUID)
}

// recapChat is the DM or group an event's call's recap went to, for the
// event's link to it; "" for a conversation's call, whose home is its own.
func (h recapHome) recapChat(delivered bool) string {
	if h.event == nil || !delivered {
		return ""
	}
	return h.conv.ID
}

// recordCallArtefacts notes on the call's transcript node what else it made
// (a seam for tests).
var recordCallArtefacts = recordingDomain.RecordCallArtefacts

// makeEventGroup makes the group of an event's people with the recap, its
// first artefact, as its first message, through the chat's own path:
// CreateChatForGroup names a new group by its member set, so it is the group
// grpID names. Its author must be one of the people.
func makeEventGroup(ctx context.Context, grpID string, authorInfo *userModels.UserInfo, htmlText string, members []*dgraphStruct.DgraphUser) error {
	author := authorInfo.UserDgraphInfo.Uuid
	if !slices.ContainsFunc(members, func(u *dgraphStruct.DgraphUser) bool { return u.Uuid == author }) {
		return fmt.Errorf("the recap's author isn't one of the event's people")
	}
	in := &chatAdapter.ChatInfo{TextHtml: htmlText}
	if _, err := chatBusiness.CreateChatForGroup(ctx, in, authorInfo, nil, members); err != nil {
		return fmt.Errorf("make the event's group: %w", err)
	}
	if in.GrpUuid != grpID {
		return fmt.Errorf("the event's group was made as %q, want %q", in.GrpUuid, grpID)
	}
	helpers.LogInfoWithContext(ctx, "AI meeting recap made the event's group %q", grpID)
	return nil
}
