package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Reading a message where it is, and not once it is deleted.

// GetDgraphPostTextWithChannel is a post's text, author and times, with the
// uuid of the channel it is in and its deletion time, for a caller to hold it
// to the channel it was asked about (channelBusiness.PostReadableIn).
func GetDgraphPostTextWithChannel(ctx context.Context, postUUID string) (*dgraphStruct.DgraphPost, error) {
	return domain.GetDgraphPostTextWithChannel(ctx, postUUID)
}

// DeletedPostText is what a deleted message's thread shows in its place.
const DeletedPostText = "<p>This message was deleted.</p>"

// ThreadForMember is a post's thread as a member opens it. A live post is the
// thread as read. A deleted one keeps its text, attachments and reactions in
// the graph, and its thread used to return them all: when replies to it are
// still there, the thread opens on a placeholder, "This message was deleted.",
// that holds the replies, its channel and when it was posted, and nothing
// else of the message (Slack keeps such a thread the same way); with no
// replies there is no thread (ok is false). post is read with its deletion
// time (GetDgraphPostByUUIDWithAllComments).
func ThreadForMember(post *dgraphStruct.DgraphPost) (thread *dgraphStruct.DgraphPost, ok bool) {
	if post == nil {
		return nil, false
	}
	if !helpers.IsSoftDeleted(post.DeletedAt) {
		return post, true
	}
	if len(post.Comments) == 0 {
		return nil, false
	}
	return &dgraphStruct.DgraphPost{
		Uid:          post.Uid,
		Uuid:         post.Uuid,
		Text:         DeletedPostText,
		Channel:      post.Channel,
		Comments:     post.Comments,
		CommentCount: post.CommentCount,
		CreatedAt:    post.CreatedAt,
		DeletedAt:    post.DeletedAt,
	}, true
}
