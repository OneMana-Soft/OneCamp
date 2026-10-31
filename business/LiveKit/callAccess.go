package business

// Who may be in a call, and who may put people out of one.
//
// The same answers the join endpoints give, asked again when it matters after
// the token was minted: when someone connects (a token outlives the right it
// was minted for), and when a call's notes are written.

import (
	"context"
	"strings"
	"time"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// ActiveMember answers the member a graph uid (a member's call identity)
// names, or nil for anyone who is not (any more) a member of this workspace.
func ActiveMember(ctx context.Context, uid string) (*dgraphStruct.DgraphUser, error) {
	if !helpers.IsDgraphUID(uid) {
		return nil, nil
	}
	u, err := userDomain.GetDgraphUserInfoByDgraphUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if u == nil || u.Uid != uid || strings.TrimSpace(u.Uuid) == "" || (u.DeletedAt != nil && u.DeletedAt.After(time.Unix(0, 0))) {
		return nil, nil
	}
	return u, nil
}

// MayOpenConversation reports whether a member (by graph uid) can open the
// conversation a call room belongs to, now: a member of the channel, one of
// the two people in the DM, a member of the group. An instant meeting belongs
// to no conversation, so the answer there is no.
func MayOpenConversation(ctx context.Context, roomName, uid string) (bool, error) {
	u, err := ActiveMember(ctx, uid)
	if err != nil || u == nil {
		return false, err
	}
	return conversationMember(ctx, roomName, u)
}

func conversationMember(ctx context.Context, roomName string, u *dgraphStruct.DgraphUser) (bool, error) {
	channelUUID, chatGroupID := helpers.ClassifyRoom(roomName)
	switch {
	case channelUUID != "":
		ch, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, u.Uid)
		if err != nil {
			return false, err
		}
		return ch != nil && (ch.IsMember == 1 || ch.IsAdmin == 1), nil
	case strings.Contains(chatGroupID, " "):
		for _, p := range strings.Fields(chatGroupID) {
			if p == u.Uuid {
				return true, nil
			}
		}
		return false, nil
	case chatGroupID != "":
		dm, err := chatDomain.GetDgraphDmBasicInfoFromDgraph(ctx, u.Uid, chatGroupID)
		if err != nil {
			return false, err
		}
		return dm != nil && dm.ParticipantIsMember == 1, nil
	}
	return false, nil
}

// MemberMayBeInCall reports whether a member may be in the call in a room
// now: someone who can open its conversation, or, in an instant meeting, the
// person who started it, or someone let in through a link they opened as
// themselves (HU13: a meeting's link, a room's address, a call's invite).
func MemberMayBeInCall(ctx context.Context, roomName, uid string) (bool, error) {
	u, err := ActiveMember(ctx, uid)
	if err != nil || u == nil {
		return false, err
	}
	var ok bool
	if strings.HasPrefix(roomName, meetingRoomPrefix) {
		ok, err = isMeetingHost(ctx, roomName, u)
	} else {
		ok, err = conversationMember(ctx, roomName, u)
	}
	if err != nil || ok {
		return ok, err
	}
	id, perr := uuid.Parse(u.Uuid)
	if perr != nil {
		return false, nil
	}
	rooms, err := RoomsAdmittedTo(ctx, id)
	return rooms[roomName], err
}

func isMeetingHost(ctx context.Context, roomName string, u *dgraphStruct.DgraphUser) (bool, error) {
	creator, ok, err := guestModel.ResourceCreator(ctx, guestModel.ResourceMeeting, roomName)
	if err != nil || !ok {
		return false, err
	}
	return creator.String() == u.Uuid, nil
}

// HostByRule reports whether someone runs the call in a room by who they are,
// before anyone hands them the part: in a channel, its managers and the
// workspace's admins; in an instant meeting, the person who started it.
// Nobody in a DM or a group: the people there are all equals in the
// conversation (D-CL-6), so none of them is a host and all of them may mute
// one another (business/CallControl). workspaceAdmin is the person's admin
// flag as their own session says it, never an API token's.
func HostByRule(ctx context.Context, roomName string, user *dgraphStruct.DgraphUser, workspaceAdmin bool) (bool, error) {
	if user == nil || user.Uid == "" {
		return false, nil
	}
	if strings.HasPrefix(roomName, meetingRoomPrefix) {
		return isMeetingHost(ctx, roomName, user)
	}
	channelUUID, _ := helpers.ClassifyRoom(roomName)
	if channelUUID == "" {
		return false, nil
	}
	ch, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, user.Uid)
	if err != nil {
		return false, err
	}
	if ch == nil {
		return false, nil
	}
	return ch.IsAdmin == 1 || workspaceAdmin, nil
}

// IsMemberIdentity reports whether a call identity is shaped like a member's
// (their graph uid), as opposed to a guest's or the agent's. Pure.
func IsMemberIdentity(identity string) bool {
	return helpers.IsDgraphUID(identity)
}

// IsWorkspaceAdmin reports whether a member (by their uuid) is one of the
// workspace's admins now, as far as this call goes: an admin's powers never
// reach a call an API token made (helpers.ActsThroughToken). Unknown, or a
// database that doesn't answer, is no.
func IsWorkspaceAdmin(ctx context.Context, userUUID string) bool {
	if helpers.ActsThroughToken(ctx) {
		return false
	}
	id, err := uuid.Parse(strings.TrimSpace(userUUID))
	if err != nil {
		return false
	}
	u, err := userDomain.GetUserByUUID(ctx, id)
	return err == nil && u != nil && u.IsAdmin
}
