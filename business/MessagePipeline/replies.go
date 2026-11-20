package business

// A thread reply tried again is the same reply, as a post or a chat is. The
// app gives a reply an id when it shows it (client_msg_id); a send with one is
// done once per person, thread and id (sendBusiness.Once, keyed by onceKey),
// so a retry after a lost answer gets the first reply's answer and writes
// nothing. With no id it just sends, as before.

import (
	"context"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// threadOnce is the place a reply in the thread under root is once in. Pure.
func threadOnce(root, me string) string {
	return onceKey(Write{To: conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: root}}, me)
}

// ReplyInPostThread writes who's reply in the thread under a channel's post,
// once per client id, and its copy in the channel when also is one (the
// route's sendBusiness.PrepareReplyCopy): the reply, its copy and their link
// are one send, so a retry posts neither twice. The copy is best-effort: a
// failure to post or link it is logged and never fails the reply.
func ReplyInPostThread(ctx context.Context, who *userModels.UserInfo, in *postAdapter.InputCreateOrUpdateCommentToPost, mentions []*dgraphStruct.DgraphUser, root *dgraphStruct.DgraphPost, also *sendBusiness.ChannelPost) (*postAdapter.OutputCreatePostCommentForPost, error) {
	me := who.UserDgraphInfo.Uuid
	return sendBusiness.Once(ctx, me, threadOnce(root.Uuid, me), in.ClientMsgID, func() (*postAdapter.OutputCreatePostCommentForPost, error) {
		reply, err := postBusiness.CreatePostComment(ctx, in, who, mentions, root)
		if err != nil || also == nil {
			return reply, err
		}
		copied, err := also.Commit(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/ReplyInPostThread also_send_to_channel failed err: %+v", err)
		} else if err := postBusiness.LinkReplyCopy(ctx, reply.Uuid, copied.Uuid); err != nil {
			// The copy is posted; only following the reply's edits is lost.
			helpers.LogErrorWithContext(ctx, "business/ReplyInPostThread Failed to link the reply to its copy err: %+v", err)
		}
		return reply, nil
	})
}

// ReplyInChatThread writes who's reply in the thread under a DM's or group's
// message, once per client id (business/Chat CreateChatComment, with the
// conversation's grouping id and the message's sender as the route read them).
func ReplyInChatThread(ctx context.Context, who *userModels.UserInfo, in *chatAdapter.InputCreateOrUpdateCommentToChat, mentions []*dgraphStruct.DgraphUser, groupingID, fromUID string, root *dgraphStruct.DgraphChat) (*chatAdapter.OutputCreateChatCommentForPost, error) {
	me := who.UserDgraphInfo.Uuid
	return sendBusiness.Once(ctx, me, threadOnce(root.Uuid, me), in.ClientMsgID, func() (*chatAdapter.OutputCreateChatCommentForPost, error) {
		return chatBusiness.CreateChatComment(ctx, in, who, mentions, groupingID, fromUID, root)
	})
}
