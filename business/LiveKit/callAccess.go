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
)

// activeMember answers the member a graph uid names, or nil for anyone who is
// not (any more) a member of this workspace.
func activeMember(ctx context.Context, uid string) (*dgraphStruct.DgraphUser, error) {
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
	u, err := activeMember(ctx, uid)
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
// person who started it (everyone else there is a guest).
func MemberMayBeInCall(ctx context.Context, roomName, uid string) (bool, error) {
	u, err := activeMember(ctx, uid)
	if err != nil || u == nil {
		return false, err
	}
	if strings.HasPrefix(roomName, meetingRoomPrefix) {
		return isMeetingHost(ctx, roomName, u)
	}
	return conversationMember(ctx, roomName, u)
}

func isMeetingHost(ctx context.Context, roomName string, u *dgraphStruct.DgraphUser) (bool, error) {
	creator, ok, err := guestModel.ResourceCreator(ctx, guestModel.ResourceMeeting, roomName)
	if err != nil || !ok {
		return false, err
	}
	return creator.String() == u.Uuid, nil
}

// MayRemoveFromCall reports whether someone may put others out of the call in
// a room: in a channel, its moderators and the workspace's admins; in an
// instant meeting, the person who started it. Nobody in a DM or a group: the
// people there are all equals in the conversation, and anyone removed could
// call straight back.
func MayRemoveFromCall(ctx context.Context, roomName string, user *dgraphStruct.DgraphUser, workspaceAdmin bool) (bool, error) {
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
