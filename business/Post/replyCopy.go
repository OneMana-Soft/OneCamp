package business

// A thread reply sent to its channel too ("Also send to #channel") is the
// reply and a copy of it posted in the channel. Edits and deletion reached the
// reply only, so the copy went on showing the old text, and a deleted reply
// stayed readable in the channel as its copy. The copy now holds an edge to
// its reply (LinkReplyCopy: post_broadcast_of), and the reply's edit and
// deletion follow it there.

import (
	"context"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// LinkReplyCopy records that the post with copyUUID is the channel copy of
// the thread reply with commentUUID.
func LinkReplyCopy(ctx context.Context, commentUUID, copyUUID string) error {
	return commentDomain.LinkReplyCopy(ctx, commentUUID, copyUUID)
}

// liveReplyCopy is the reply's copy in its channel while it is there, or nil.
func liveReplyCopy(ctx context.Context, commentUUID string) *dgraphStruct.DgraphPost {
	cp, err := commentDomain.GetReplyCopy(ctx, commentUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/liveReplyCopy Failed to read the reply's copy err: %+v", err)
		return nil
	}
	if cp == nil || cp.Uuid == "" || helpers.IsSoftDeleted(cp.DeletedAt) || cp.Channel == nil || cp.PostBy == nil {
		return nil
	}
	return cp
}

// mirrorReplyEdit gives the reply's copy, when it has one, the reply's new
// text. The copy names nobody of its own (sendBusiness WithoutMentions).
func mirrorReplyEdit(ctx context.Context, commentUUID, html string) {
	cp := liveReplyCopy(ctx, commentUUID)
	if cp == nil {
		return
	}
	id, err := uuid.Parse(cp.Uuid)
	if err != nil {
		return
	}
	if err := UpdatePost(ctx, &adapter.InputCreateOrUpdatePostInfo{Uuid: cp.Uuid, HTMLText: html, ChannelUuid: cp.Channel.Uuid},
		nil, id, cp.Channel.Uuid, cp.PostBy.Uuid); err != nil {
		helpers.LogErrorWithContext(ctx, "business/mirrorReplyEdit Failed to edit the reply's copy err: %+v", err)
	}
}

// mirrorReplyDelete deletes the reply's copy, when it has one.
func mirrorReplyDelete(ctx context.Context, commentUUID string) {
	cp := liveReplyCopy(ctx, commentUUID)
	if cp == nil {
		return
	}
	full, err := domain.GetDgraphPostByUUID(ctx, cp.Uuid, "")
	if err != nil || full == nil {
		helpers.LogErrorWithContext(ctx, "business/mirrorReplyDelete Failed to read the reply's copy err: %+v", err)
		return
	}
	if err := DeletePost(ctx, full, cp.Channel.Uuid); err != nil {
		helpers.LogErrorWithContext(ctx, "business/mirrorReplyDelete Failed to delete the reply's copy err: %+v", err)
	}
}
