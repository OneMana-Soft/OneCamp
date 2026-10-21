package businness

// Reactions on a doc's comments (chat B25's one rule): once per person and
// emoji through business/Reaction, and the doc's open tabs are told only of
// what really changed.

import (
	"context"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// CreateOrUpdateDocCommentReaction adds a reaction to a doc's comment as
// userDgraph, once per emoji. The answer is the reaction's node, the one
// added or the one the person already had.
func CreateOrUpdateDocCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInDoc, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: reactionInfo.CommentId}
	if dgraphCommentRaw.CommentBy != nil {
		parent.ContentBy = dgraphCommentRaw.CommentBy.Uid
	}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraph.Uid, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiReactionUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdateDocCommentReaction err: %+v", err)
		}
		return "", err
	}
	tellDocCommentReactionsGone(dgraphCommentRaw, userDgraph.Uuid, removed)
	if added.Created || added.Retagged {
		live := &mqttStruct.MqttDocCommentReaction{
			Type: added.Told(), EmojiReactionId: reactionInfo.EmojiReactionUuid, CommentUuid: reactionInfo.CommentId,
			AddedByUserName: userDgraph.DisplayName(), AddedByUuid: userDgraph.Uuid, ReactionUuid: added.UID,
			DocUuid: dgraphCommentRaw.Doc.Uuid,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishDocCommentReaction", func() { mqttBusiness.PublishDocCommentReaction(live, live.DocUuid) })
	}
	return added.UID, nil
}

// tellDocCommentReactionsGone tells the doc's open tabs of each reaction
// taken back, and whose it was: one live event apiece, as the apps read them.
func tellDocCommentReactionsGone(comment *dgraphStruct.DgraphComment, byUUID string, removed []reactionBusiness.Removed) {
	for _, r := range removed {
		gone := &mqttStruct.MqttDocCommentReaction{
			Type: mqttStruct.TYPE_DELETE, CommentUuid: comment.Uuid, ReactionUuid: r.UID,
			EmojiReactionId: r.Emoji, DocUuid: comment.Doc.Uuid, AddedByUuid: byUUID,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishDocCommentReaction", func() { mqttBusiness.PublishDocCommentReaction(gone, gone.DocUuid) })
	}
}

// DeleteReactionOnCommentDoc takes back userDgraph's reaction reactionUID on a
// doc's comment, with any copy of the same emoji of theirs.
func DeleteReactionOnCommentDoc(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionUID string, userDgraph *dgraphStruct.DgraphUser) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: commentDgraph.Uuid}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraph.Uid, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeleteReactionOnCommentDoc err: %+v", err)
		}
		return err
	}
	tellDocCommentReactionsGone(commentDgraph, userDgraph.Uuid, removed)
	return nil
}
