package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
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

// PostReadableIn reports whether post may be shown to someone who asked for it
// in channel, as read for them: they can read the channel (CanRead), the post
// is in that channel, and it isn't deleted. A post's id alone says nothing
// about who may read it, so a post is always held to the channel it is in:
// read by its id under any channel the asker could read, a private channel's
// posts, and deleted ones, were readable by anyone who had one of their ids.
// post is read with its channel's uuid (post_channel) and its deletion time.
func PostReadableIn(channel *dgraphStruct.DgraphChannel, post *dgraphStruct.DgraphPost) bool {
	return CanRead(channel) && post != nil && post.Channel != nil &&
		post.Channel.Uuid == channel.Uuid && !helpers.IsSoftDeleted(post.DeletedAt)
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

// WritableBy reports whether the person with userUUID may write in the
// channel, as a thread reply or a file does: they are one of its members and
// it isn't archived. Writing at its top level also takes the announcement
// rule (business/Send MayPostIn). A lookup that fails, and a channel that
// isn't there (ErrNotFound), are errors, as for ReadableBy.
func WritableBy(ctx context.Context, userUUID, channelID string) (bool, error) {
	ch, err := readAs(ctx, userUUID, channelID)
	if err != nil || ch == nil {
		return false, err
	}
	return ch.IsMember > 0 && !helpers.IsSoftDeleted(ch.DeletedAt), nil
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
