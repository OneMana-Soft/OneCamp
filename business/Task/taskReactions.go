package business

// Reactions on a task's comments (chat B25's one rule): once per person and
// emoji through business/Reaction, and the project's open views are told only
// of what really changed.

import (
	"context"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// CreateOrUpdateTaskCommentReaction adds a reaction to a task's comment as
// userDgraph, once per emoji. The answer is the reaction's node, the one
// added or the one the person already had.
func CreateOrUpdateTaskCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInTask, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: reactionInfo.Uuid}
	if dgraphCommentRaw.CommentBy != nil {
		parent.ContentBy = dgraphCommentRaw.CommentBy.Uid
	}
	added, removed, err := reactionBusiness.Set(ctx, parent, userDgraph.Uid, reactionInfo.ReactionDgraphUid, reactionInfo.EmojiUuid)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/CreateOrUpdateTaskCommentReaction err: %+v", err)
		}
		return "", err
	}
	tellTaskCommentReactionsGone(dgraphCommentRaw, userDgraph.Uuid, removed)
	if added.Created || added.Retagged {
		live := &mqttStruct.MqttTaskCommentReaction{
			Type: added.Told(), CommentUuid: reactionInfo.Uuid, EmojiReactionId: reactionInfo.EmojiUuid,
			AddedByUuid: userDgraph.Uuid, AddedByUserName: userDgraph.DisplayName(), ReactionUuid: added.UID,
			TaskUuid: dgraphCommentRaw.Task.Uuid,
		}
		project := dgraphCommentRaw.Task.Project.Uuid
		helpers.GoSafeNamed("mqttBusiness.PublishTaskCommentReaction", func() { mqttBusiness.PublishTaskCommentReaction(live, project) })
	}
	return added.UID, nil
}

// tellTaskCommentReactionsGone tells the project's open views of each
// reaction taken back, and whose it was: one live event apiece, as the apps
// read them.
func tellTaskCommentReactionsGone(comment *dgraphStruct.DgraphComment, byUUID string, removed []reactionBusiness.Removed) {
	project := comment.Task.Project.Uuid
	for _, r := range removed {
		gone := &mqttStruct.MqttTaskCommentReaction{
			Type: mqttStruct.TYPE_DELETE, CommentUuid: comment.Uuid, AddedByUuid: byUUID,
			ReactionUuid: r.UID, EmojiReactionId: r.Emoji, TaskUuid: comment.Task.Uuid,
		}
		helpers.GoSafeNamed("mqttBusiness.PublishTaskCommentReaction", func() { mqttBusiness.PublishTaskCommentReaction(gone, project) })
	}
}

// DeleteReactionOnCommentTask takes back userDgraph's reaction reactionUID on
// a task's comment, with any copy of the same emoji of theirs.
func DeleteReactionOnCommentTask(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionUID string, userDgraph *dgraphStruct.DgraphUser) error {
	parent := reactionBusiness.Parent{On: reactionBusiness.OnComment, UUID: commentDgraph.Uuid}
	removed, err := reactionBusiness.RemoveMine(ctx, parent, userDgraph.Uid, reactionUID)
	if err != nil {
		if reactionBusiness.Fault(err) {
			helpers.LogErrorWithContext(ctx, "business/DeleteReactionOnCommentTask err: %+v", err)
		}
		return err
	}
	tellTaskCommentReactionsGone(commentDgraph, userDgraph.Uuid, removed)
	return nil
}
