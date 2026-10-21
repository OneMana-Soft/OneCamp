package business

// Reactions on a channel's messages and on their replies (chat B25): written
// once per person and emoji through business/Reaction, and told only for what
// really changed. An app from before several reactions still sends its one
// reaction's node to change it (reaction_dgraph_id): that one goes and the new
// emoji comes, as two changes, so every app shows the same.

import (
	"context"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
)

// CreateOrUpdatePostReaction adds a reaction to a channel's message as the
// person userDgraphUID, once per emoji, and tells the channel and the
// message's author of what really changed. The answer is the reaction's node,
// the one added or the one the person already had.
func CreateOrUpdatePostReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForPost, postCreatedByDgraphUID string, postOwnerUUID string, userDgraphUID string, userUUID string, userName string, channelId string) (reactionUid string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnPost, UUID: reactionInfo.Uuid, ContentBy: postCreatedByDgraphUID}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraphUID, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdatePostReaction err: %+v", err)
		}
		return "", err
	}
	if helpers.IsBulkImport(ctx) {
		return added.UID, nil
	}
	tellPostReactionsGone(reactionInfo.Uuid, userUUID, channelId, removed)
	if !added.Created && !added.Retagged {
		return added.UID, nil
	}
	live := &mqttStruct.MqttPostReaction{
		Type: added.Told(), EmojiReactionId: reactionInfo.EmojiUuid, PostUuid: reactionInfo.Uuid,
		AddedByUuid: userUUID, AddedByUserName: userName, ChannelUuid: channelId, ReactionUuid: added.UID,
	}
	helpers.GoSafeNamed("mqttBusiness.PublishPostReaction", func() { mqttBusiness.PublishPostReaction(live, channelId) })

	// The message's author hears of it in their activity.
	if postOwnerUUID != userUUID {
		currentTime := time.Now()
		item := &dgraphModels.UnifiedActivityItem{
			ActivityType: "POST_REACTION",
			Time:         currentTime.UTC().Format(time.RFC3339Nano),
			Reaction: &dgraphModels.ReactionsActivity{
				DgraphReaction: dgraphStruct.DgraphReaction{
					EmojiUuid: reactionInfo.EmojiUuid,
					AddedBy:   &dgraphStruct.DgraphUser{Uuid: userUUID, UserName: userName},
					AddedAt:   &currentTime,
				},
				Post: &dgraphStruct.DgraphPost{
					Uuid:    reactionInfo.Uuid,
					Channel: &dgraphStruct.DgraphChannel{Uuid: channelId},
				},
			},
		}
		helpers.GoSafeNamed("activityBusiness.PublishActivityToUser", func() { activityBusiness.PublishActivityToUser(postOwnerUUID, item) })
	}
	return added.UID, nil
}

// tellPostReactionsGone tells the channel of each reaction taken back, one
// live event apiece as the apps read them (usually one: a person holds one
// copy of an emoji).
func tellPostReactionsGone(postUUID, byUUID, channelID string, removed []reactionBusiness.Removed) {
	for _, r := range removed {
		gone := &mqttStruct.MqttPostReaction{
			Type: mqttStruct.TYPE_DELETE, PostUuid: postUUID, AddedByUuid: byUUID,
			ChannelUuid: channelID, ReactionUuid: r.UID, EmojiReactionId: r.Emoji,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishPostReaction", func() { mqttBusiness.PublishPostReaction(gone, channelID) })
	}
}

// DeletePostReaction takes back the person's reaction reactionUID on a
// channel's message, with any copy of the same emoji of theirs, and tells the
// channel.
func DeletePostReaction(ctx context.Context, postUUID, reactionUID, userDgraphUID, userUUID, channelID string) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnPost, UUID: postUUID}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraphUID, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeletePostReaction err: %+v", err)
		}
		return err
	}
	tellPostReactionsGone(postUUID, userUUID, channelID, removed)
	return nil
}

// CreateOrUpdatePostCommentReaction adds a reaction to a reply in a channel's
// thread as userDgraph, once per emoji, and tells the channel, the reply's
// author and the message's author of what really changed.
func CreateOrUpdatePostCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInPost, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: reactionInfo.Uuid}
	if dgraphCommentRaw.CommentBy != nil {
		parent.ContentBy = dgraphCommentRaw.CommentBy.Uid
	}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraph.Uid, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdatePostCommentReaction err: %+v", err)
		}
		return "", err
	}

	// A bulk Slack import writes the reaction and tells nobody: 50,000
	// historical reactions must not become as many live items.
	if helpers.IsBulkImport(ctx) {
		return added.UID, nil
	}
	tellPostCommentReactionsGone(dgraphCommentRaw, removed)
	if !added.Created && !added.Retagged {
		return added.UID, nil
	}
	live := &mqttStruct.MqttPostCommentReaction{
		Type: added.Told(), EmojiReactionId: reactionInfo.EmojiUuid, CommentUuid: reactionInfo.Uuid,
		AddedByUserName: userDgraph.DisplayName(), AddedByUuid: userDgraph.Uuid, ReactionUuid: added.UID,
		PostUuid: dgraphCommentRaw.Post.Uuid,
	}
	channelID := dgraphCommentRaw.Post.Channel.Uuid
	helpers.GoSafeNamed("mqttBusiness.PublishPostCommentReaction", func() { mqttBusiness.PublishPostCommentReaction(live, channelID) })

	// The reply's author and the message's author hear of it in their
	// activity, the reactor never.
	currentTime := time.Now()
	helpers.GoSafeNamed("business/Post/postReactions:CreateOrUpdatePostCommentReaction", func() {
		recipients := map[string]bool{}
		if dgraphCommentRaw.CommentBy != nil && dgraphCommentRaw.CommentBy.Uuid != userDgraph.Uuid {
			recipients[dgraphCommentRaw.CommentBy.Uuid] = true
		}
		if by := dgraphCommentRaw.Post.PostBy; by != nil && by.Uuid != userDgraph.Uuid && (dgraphCommentRaw.CommentBy == nil || by.Uuid != dgraphCommentRaw.CommentBy.Uuid) {
			recipients[by.Uuid] = true
		}
		for recipientID := range recipients {
			activityBusiness.PublishActivityToUser(recipientID, &dgraphModels.UnifiedActivityItem{
				ActivityType: "REACTION",
				Time:         currentTime.UTC().Format(time.RFC3339Nano),
				Reaction: &dgraphModels.ReactionsActivity{
					DgraphReaction: dgraphStruct.DgraphReaction{
						EmojiUuid: reactionInfo.EmojiUuid,
						AddedBy:   &dgraphStruct.DgraphUser{Uuid: userDgraph.Uuid, UserName: userDgraph.DisplayName()},
						AddedAt:   &currentTime,
					},
					Comment: &dgraphStruct.DgraphComment{
						Uuid: reactionInfo.Uuid,
						Post: &dgraphStruct.DgraphPost{
							Uuid:    dgraphCommentRaw.Post.Uuid,
							Channel: &dgraphStruct.DgraphChannel{Uuid: dgraphCommentRaw.Post.Channel.Uuid},
						},
					},
				},
			})
		}
	})
	return added.UID, nil
}

// tellPostCommentReactionsGone tells the channel of each reaction taken back
// from a reply, one live event apiece as the apps read them.
func tellPostCommentReactionsGone(comment *dgraphStruct.DgraphComment, removed []reactionBusiness.Removed) {
	channelID := comment.Post.Channel.Uuid
	for _, r := range removed {
		gone := &mqttStruct.MqttPostCommentReaction{
			Type: mqttStruct.TYPE_DELETE, CommentUuid: comment.Uuid, ReactionUuid: r.UID,
			EmojiReactionId: r.Emoji, PostUuid: comment.Post.Uuid,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishPostCommentReaction", func() { mqttBusiness.PublishPostCommentReaction(gone, channelID) })
	}
}

// DeleteReactionOnCommentPost takes back the person's reaction reactionUID on
// a reply, with any copy of the same emoji of theirs, and tells the channel.
func DeleteReactionOnCommentPost(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionUID, userDgraphUID string) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: commentDgraph.Uuid}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraphUID, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeleteReactionOnCommentPost err: %+v", err)
		}
		return err
	}
	tellPostCommentReactionsGone(commentDgraph, removed)
	return nil
}
