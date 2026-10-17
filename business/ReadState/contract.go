// Package business (ReadState) is the one read state for conversations,
// threads and the Inbox (design/chat-vs-slack/PROGRAM.md §4.2 #24 and CH11;
// A3 builds it): markers at message precision, every count from one place, and
// one live read.moved event.
//
// DAY 0. Today's markers, behind the same names: a channel's last_seen_channel
// row (business/Channel MarkChannelSeen, as POST /ch/markSeen moves it, for
// someone the channel kind lets read it) and a DM's or group's last_seen_chat
// row with its read receipt (business/Chat MarkChatSeen, as POST /dm/seen and
// /groupChat/seen); the counts are the sidebar's
// (GetDgraphUserInfoByUUIDForSidebarNav). Threads and the Inbox have no marker
// of their own yet. Nothing calls States yet, so nothing changes by its
// existing.
package business

import (
	"context"
	"errors"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Count is what someone hasn't read in one conversation.
type Count struct {
	Conversation conversationBusiness.Ref `json:"conversation"`
	// Unread is its messages after their marker, not their own, live; thread
	// replies never count.
	Unread uint64 `json:"unread"`
}

// Reader keeps who has read what.
type Reader interface {
	// MarkRead marks ref read for who, up to now. ErrNotFound for a
	// conversation they can't read.
	MarkRead(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) error
	// Counts is who's count for every conversation they're in.
	Counts(ctx context.Context, who *userModels.UserInfo) ([]Count, error)
}

// States is the read state everything asks.
var States Reader = todaysMarkers{}

// todaysMarkers are today's last_seen_* rows.
type todaysMarkers struct{}

func (todaysMarkers) MarkRead(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) error {
	if who == nil {
		return conversationBusiness.ErrNotFound
	}
	switch ref.Kind {
	case conversationBusiness.KindChannel:
		id, err := uuid.Parse(ref.ID)
		if err != nil {
			return conversationBusiness.ErrNotFound
		}
		if _, err := conversationBusiness.Resolve(ctx, who, ref); err != nil {
			return err
		}
		return channelBusiness.MarkChannelSeen(ctx, who.UserPostgresInfo.Id, id)
	case conversationBusiness.KindDM, conversationBusiness.KindGroup:
		err := chatBusiness.MarkChatSeen(ctx, who, ref.ID)
		if errors.Is(err, chatBusiness.ErrNotInChat) {
			// A DM of one's own that has no message yet is read already.
			if ref.Other(who.UserDgraphInfo.Uuid) != "" {
				return nil
			}
			return conversationBusiness.ErrNotFound
		}
		return err
	}
	return conversationBusiness.ErrNotFound
}

func (todaysMarkers) Counts(ctx context.Context, who *userModels.UserInfo) ([]Count, error) {
	if who == nil {
		return nil, conversationBusiness.ErrNotFound
	}
	id, err := uuid.Parse(who.UserDgraphInfo.Uuid)
	if err != nil {
		return nil, conversationBusiness.ErrNotFound
	}
	user, err := userBusiness.GetDgraphUserInfoByUUIDForSidebarNav(ctx, id)
	if err != nil {
		return nil, err
	}
	return countsOf(user), nil
}

// countsOf is the sidebar's counts, one per conversation. Pure.
func countsOf(user *dgraphStruct.DgraphUser) []Count {
	if user == nil {
		return nil
	}
	out := make([]Count, 0, len(user.Channels)+len(user.DMs))
	for _, c := range user.Channels {
		if c != nil && c.Uuid != "" {
			out = append(out, Count{Conversation: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: c.Uuid}, Unread: c.UnreadPostCount})
		}
	}
	for _, d := range user.DMs {
		if d == nil || d.GroupingId == "" {
			continue
		}
		out = append(out, Count{Conversation: conversationBusiness.ChatRef(d.GroupingId), Unread: d.UnreadMessageCount})
	}
	return out
}
