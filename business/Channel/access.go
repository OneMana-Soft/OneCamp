package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CanRead is who may read a channel's messages: anyone signed in, for a
// public channel; its members and moderators, for a private one. A channel
// whose privacy didn't load is private. channel is as read for the person
// asking: its member and moderator counts are theirs.
func CanRead(channel *dgraphStruct.DgraphChannel) bool {
	if channel == nil || channel.Uuid == "" {
		return false
	}
	if channel.IsMember > 0 || channel.IsAdmin > 0 {
		return true
	}
	return channel.IsPrivate != nil && !*channel.IsPrivate
}

// ReadableBy reports whether the person with userUUID can read the channel's
// messages (CanRead). Someone who isn't an active person reads nothing.
func ReadableBy(ctx context.Context, userUUID, channelID string) (bool, error) {
	ch, err := readAs(ctx, userUUID, channelID)
	if err != nil || ch == nil {
		return false, err
	}
	return CanRead(ch), nil
}

// IsPublic reports whether everyone signed in can read the channel, asked as
// the person with userUUID. A channel whose privacy didn't load is private.
func IsPublic(ctx context.Context, userUUID, channelID string) (bool, error) {
	ch, err := readAs(ctx, userUUID, channelID)
	if err != nil || ch == nil {
		return false, err
	}
	return ch.IsPrivate != nil && !*ch.IsPrivate, nil
}

// readAs is the channel as the person with userUUID sees it, or nil when they
// aren't an active person or there's no such channel.
func readAs(ctx context.Context, userUUID, channelID string) (*dgraphStruct.DgraphChannel, error) {
	user, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil || user == nil || user.Uid == "" {
		return nil, err
	}
	ch, err := domain.GetBasicDgraphChannelInfoByUUID(ctx, channelID, user.Uid)
	if err != nil || ch == nil || ch.Uuid != channelID {
		return nil, err
	}
	return ch, nil
}
