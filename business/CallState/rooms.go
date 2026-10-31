package business

// Which call a LiveKit room is, and whether it is this workspace's.
//
// A room's name says what the call belongs to (helpers.ClassifyRoom): a
// channel's uuid, a DM's grouping id (its two people's uuids, a space
// between), a group's grouping id (a hash), or an instant meeting's
// "meet-<id>".

import (
	"context"
	"strings"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	conversationModel "github.com/akashc777/OneCamp/models/postgres/Conversation"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// meetingRoomPrefix starts an instant meeting's room (business/Guest's
// MeetingRoomPrefix).
const meetingRoomPrefix = "meet-"

// RefOfRoom answers what a room's call belongs to and its kind (huddle,
// meeting or breakout), or false for a name no call uses. A breakout room
// belongs to what its parent call does. Pure.
func RefOfRoom(room string) (Ref, string, bool) {
	room = strings.TrimSpace(room)
	if parent, _, ok := ParentOfBreakout(room); ok {
		ref, _, _ := refOfCallRoom(parent)
		return ref, model.KindBreakout, true
	}
	return refOfCallRoom(room)
}

// refOfCallRoom is RefOfRoom for a call's own room, never a breakout's. Pure.
func refOfCallRoom(room string) (Ref, string, bool) {
	if id, ok := strings.CutPrefix(room, meetingRoomPrefix); ok {
		if id == "" {
			return Ref{}, "", false
		}
		return Ref{Kind: KindMeeting, ID: id}, model.KindMeeting, true
	}
	channelUUID, groupingID := helpers.ClassifyRoom(room)
	switch {
	case channelUUID != "":
		if _, err := uuid.Parse(channelUUID); err != nil {
			return Ref{}, "", false
		}
		return Ref{Kind: conversationBusiness.KindChannel, ID: channelUUID}, model.KindHuddle, true
	case groupingID != "" && validGrouping(groupingID):
		return conversationBusiness.ChatRef(groupingID), model.KindHuddle, true
	}
	return Ref{}, "", false
}

// validGrouping is a DM's grouping id (its two people's uuids, a space
// between: helpers.GetGroupingId) or a group's (32 hex characters of a
// sha256), and nothing else that happens to have a space in it. Pure.
func validGrouping(id string) bool {
	if strings.Contains(id, " ") {
		parts := strings.Fields(id)
		if len(parts) != 2 {
			return false
		}
		for _, p := range parts {
			if _, err := uuid.Parse(p); err != nil {
				return false
			}
		}
		return true
	}
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// RoomOf is the LiveKit room of what a call belongs to. Pure.
func RoomOf(ref Ref) string {
	if ref.Kind == KindMeeting {
		return meetingRoomPrefix + ref.ID
	}
	return ref.ID
}

// BelongsHere reports whether a room's call is this workspace's. A LiveKit
// server shared between workspaces tells every one of them about every room,
// and a call that isn't ours must change nothing here and nobody there.
//
// A group's room is a hash of its people, which only the conversations index
// can recognise: until the index has been filled in from the graph (once,
// after an upgrade), every group room is taken as ours, as it always was.
func BelongsHere(ctx context.Context, room string) bool {
	if parent, _, ok := ParentOfBreakout(room); ok {
		room = parent
	}
	ref, _, ok := RefOfRoom(room)
	if !ok {
		return false
	}
	switch ref.Kind {
	case KindMeeting:
		exists, err := guestModel.ResourceExists(ctx, room)
		return err == nil && exists
	case conversationBusiness.KindChannel:
		id, err := uuid.Parse(ref.ID)
		if err != nil {
			return false
		}
		ch, err := channelDomain.GetChannelInfoByUUID(ctx, id)
		return err == nil && ch != nil
	case conversationBusiness.KindDM:
		// Ours when either of its people is ours: with one person in another
		// workspace (a shared server's DM can't be) either answer is the same.
		for _, p := range strings.Fields(ref.ID) {
			id, err := uuid.Parse(p)
			if err != nil {
				continue
			}
			if u, err := userDomain.GetUserByUUID(ctx, id); err == nil && u != nil {
				return true
			}
		}
		return false
	case conversationBusiness.KindGroup:
		if done, err := configModel.IsDone(conversationModel.BackfillKey); err != nil || !done {
			return true
		}
		c, err := conversationModel.ByRef(ctx, ref.ID)
		return err == nil && c != nil
	}
	return false
}
