package business

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// Breakout rooms (calls package C, meet-10 and zoom-02): a channel's call or
// a meeting splits into numbered rooms, each a call of its own in LiveKit
// whose session is its parent's (parent_session_id). A room is named from
// its parent's room, so every room-keyed path (the webhooks, BelongsHere,
// RefOfRoom) knows whose call it is. A DM's or a group's call has none.

// MaxBreakouts is the most rooms a call may split into.
const MaxBreakouts = 50

// breakoutMark joins a room's number to its parent's room: no call's own room
// has it (a channel's uuid, a meeting's "meet-<id>").
const breakoutMark = "~b"

// BreakoutRoom is the LiveKit room of room n of the call in parent's room. Pure.
func BreakoutRoom(parent string, n int) string {
	return parent + breakoutMark + strconv.Itoa(n)
}

// ParentOfBreakout answers whose call a breakout room is, and its number:
// a channel's or a meeting's own room, then the mark and a number from 1 to
// MaxBreakouts written plainly. Anything else is not a breakout room. Pure.
func ParentOfBreakout(room string) (string, int, bool) {
	i := strings.LastIndex(room, breakoutMark)
	if i <= 0 {
		return "", 0, false
	}
	parent, num := room[:i], room[i+len(breakoutMark):]
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > MaxBreakouts || strconv.Itoa(n) != num {
		return "", 0, false
	}
	ref, _, ok := refOfCallRoom(parent)
	if !ok || (ref.Kind != conversationBusiness.KindChannel && ref.Kind != KindMeeting) {
		return "", 0, false
	}
	return parent, n, true
}

// IsBreakoutRoom is whether a LiveKit room is one of a call's breakout rooms. Pure.
func IsBreakoutRoom(room string) bool {
	_, _, ok := ParentOfBreakout(room)
	return ok
}

func init() {
	// The transcription switch is the whole call's (business/LiveKit's
	// SetTranscription), so it asks here which rooms a call has; so does the
	// call's dot (IsCallActive), lit while a person is in any of them, and the
	// sidebar's asks whose call a breakout room is.
	liveKitBusiness.OnCallRooms(roomsOfCall)
	liveKitBusiness.OnCallOwnRoom(ownRoomOf)
}

// ownRoomOf is the own room of the call going on in a room: a breakout room's
// parent's, any other room itself. Pure.
func ownRoomOf(room string) string {
	if parent, _, ok := ParentOfBreakout(room); ok {
		return parent
	}
	return room
}

// roomsOfCall is every LiveKit room of the call going on in a room: the
// call's own room first, then its open breakout rooms. A room with no call
// going on in it is a call of its own.
func roomsOfCall(ctx context.Context, room string) ([]string, error) {
	own := ownRoomOf(room)
	rooms := []string{own}
	call, err := model.OpenSessionOf(ctx, own)
	if err != nil {
		return nil, err
	}
	if call != nil {
		open, err := model.OpenRoomsOf(ctx, call.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range open {
			rooms = append(rooms, r.Room)
		}
	}
	if room != own && !slices.Contains(rooms, room) {
		rooms = append(rooms, room)
	}
	return rooms, nil
}

// OpenRoomsOf answers the calls of a call's open breakout rooms (none for a
// call with none, or for a room's own call).
func OpenRoomsOf(ctx context.Context, s *Session) ([]*Session, error) {
	id, err := uuid.Parse(s.ID)
	if err != nil {
		return nil, nil
	}
	open, err := model.OpenRoomsOf(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]*Session, 0, len(open))
	for i := range open {
		r, err := view(ctx, &open[i])
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// InRoom answers the call going on in a room, a call's own or one of its
// breakout rooms, or nil.
func InRoom(ctx context.Context, room string) (*Session, error) {
	open, err := model.OpenSessionOf(ctx, room)
	if err != nil || open == nil {
		return nil, err
	}
	return view(ctx, open)
}

// OpenRoom answers the call of room n of a live call's breakout rooms, making
// it when there is none: a room's token is minted beside it, so that its
// answer names the room's call before anyone is in it. A room's call made here
// holds what its parent holds.
func OpenRoom(ctx context.Context, parent *Session, n int) (*Session, error) {
	parentID, err := uuid.Parse(parent.ID)
	if err != nil {
		return nil, ErrNotACallRoom
	}
	open, made, err := openFor(ctx, parent.Resource, model.KindBreakout, JoinEvent{Room: BreakoutRoom(parent.Room, n), At: time.Now()}, &parentID)
	if err != nil {
		return nil, err
	}
	if made {
		if err := model.TakeParentsHolds(ctx, open.ID, parentID); err != nil {
			return nil, err
		}
	}
	return sessionByID(ctx, open.ID)
}
