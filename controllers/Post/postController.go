package controllers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	messagePipeline "github.com/akashc777/OneCamp/business/MessagePipeline"
	business "github.com/akashc777/OneCamp/business/Post"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	channelDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	postDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Post"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func CreatePost(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var postInfo adapter.InputCreateOrUpdatePostInfo

	err := json.NewDecoder(r.Body).Decode(&postInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreatePost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}
	// The message pipeline (chat A2): business/Send's rules, once per id the
	// app gave it (Try again after a lost answer is the same post, not a
	// second one), and the write, durable when MESSAGE_PIPELINE switches
	// channels on, today's otherwise. The route is its thin adapter.
	written, err := messagePipeline.Messages.Write(ctx, &userInfo, messagePipeline.Write{
		Op: messagePipeline.OpSend,
		To: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: postInfo.ChannelUuid},
		Draft: messagePipeline.Draft{
			HTML: postInfo.HTMLText, Files: postInfo.MediaObj, ReplyTo: postInfo.ReplyToUUID, ClientMsgID: postInfo.ClientMsgID,
		},
	})
	cratedPostInfo := &adapter.OutputCreatePostForPost{Uuid: written.ID, PostCreated: written.At}
	if rj, ok := sendBusiness.AsRejection(err); ok {
		writeSendError(w, rj)
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreatePost Failed to create post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get Failed to create post",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created post successfully!", "data": cratedPostInfo})

}

func DeletePostCommentReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var commentReactionInfo adapter.InputDeleteReactionForPostComment

	err := json.NewDecoder(r.Body).Decode(&commentReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostCommentReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentInfo, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, commentReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || commentInfo == nil || commentInfo.Post == nil || commentInfo.Post.Channel == nil || !commentInfo.Post.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostCommentReaction Failed to get dgraph comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't remove the reaction. Try again.",
			"err": err,
		})
		return
	}

	if commentInfo.Post.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	reactionDgraph, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, commentInfo.Uid, commentReactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
	if refusal != nil {
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
		return
	}

	err = business.DeleteReactionOnCommentPost(ctx, commentInfo, reactionDgraph.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostCommentReaction Failed to delete post on the comment req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	// It answered nothing on success: an empty 200 a client can't tell from a failure.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted reaction successfully!"})
}

func DeletePostReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var postReactionInfo adapter.InputDeleteReactionForPost
	err := json.NewDecoder(r.Body).Decode(&postReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	postInfo, err := business.GetDgraphPostByUUID(ctx, postReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || !postInfo.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReaction Failed to get post info from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't remove the reaction. Try again.",
			"err": err,
		})
		return
	}

	if postInfo.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	reactionDgraphInfo, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnPost, postInfo.Uid, postReactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
	if refusal != nil {
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
		return
	}

	err = business.DeletePostReaction(ctx, postInfo.Uid, reactionDgraphInfo.Uid, postInfo.PostBy.Uid, userInfo.UserDgraphInfo.Uid, postInfo.Uuid, userInfo.UserDgraphInfo.Uuid, postInfo.Channel.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted reaction successfully!"})

}

func CreateOrUpdateReactionOnCommentPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentPostReactionInfo adapter.InputUpdateReactionForCommentInPost

	err := json.NewDecoder(r.Body).Decode(&commentPostReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentPost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, commentPostReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphComment == nil || !dgraphComment.Post.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentPost Failed to comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add reaction",
			"err": err,
		})
		return
	}

	if dgraphComment.Post.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized ",
		})
		return
	}

	// A uid changes that reaction, and only the caller's own on this comment.
	if commentPostReactionInfo.ReactionDgraphUid != "" {
		reactionDgraph, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, dgraphComment.Uid, commentPostReactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
		if refusal != nil {
			helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
			return
		}
		commentPostReactionInfo.ReactionDgraphUid = reactionDgraph.Uid
	}

	// A new reaction answers with its uid; a changed one keeps its own. The
	// write answers a uid only for a node it made, so a change answered none,
	// and the reply's reaction lost its id in the answer.
	created, err := business.CreateOrUpdatePostCommentReaction(ctx, &commentPostReactionInfo, dgraphComment, &userInfo.UserDgraphInfo)
	if created != "" {
		commentPostReactionInfo.ReactionDgraphUid = created
	}

	if err != nil || commentPostReactionInfo.ReactionDgraphUid == "" {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReactionOnCommentPost Failed to create/update reaction on post comment req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated reaction successfully!", "data": commentPostReactionInfo})
}

func CreateOrUpdateReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var postReactionInfo adapter.InputUpdateReactionForPost
	err := json.NewDecoder(r.Body).Decode(&postReactionInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	postInfo, err := business.GetDgraphPostByUUID(ctx, postReactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || !postInfo.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReaction Failed to dgraph post by uuid err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't add the reaction. Try again.",
			"err": err,
		})
		return
	}

	if postInfo.Channel.IsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized ",
		})
		return
	}

	// A deleted message takes no reactions.
	if helpers.IsSoftDeleted(postInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	// A uid changes that reaction, and only the caller's own on this post.
	if postReactionInfo.ReactionDgraphUid != "" {
		reactionDgraph, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnPost, postInfo.Uid, postReactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
		if refusal != nil {
			helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
			return
		}
		postReactionInfo.ReactionDgraphUid = reactionDgraph.Uid
	}

	postReactionInfo.ReactionDgraphUid, err = business.CreateOrUpdatePostReaction(ctx, &postReactionInfo, postInfo.PostBy.Uid, postInfo.PostBy.Uuid, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(), postInfo.Channel.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateReaction Failed to update reaction err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update reaction",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated reaction successfully!", "data": postReactionInfo})
}

func UpdatePost(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var postInfo adapter.InputCreateOrUpdatePostInfo

	err := json.NewDecoder(r.Body).Decode(&postInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdatePost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}
	postUUID, err := uuid.Parse(postInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdatePost Failed to parse postUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse postUUID in req",
			"err": err,
		})
		return
	}

	dgraphPost, err := business.GetDgraphPostByUUID(ctx, postInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || !dgraphPost.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdatePost Failed to get post info from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post info of given postId",
			"err": err,
		})
		return
	}

	// Only a post's author edits it, while they're in its channel. This
	// allowed anyone in the channel: their edit then showed under the
	// author's name.
	if dgraphPost.PostBy == nil || dgraphPost.PostBy.Uuid != userInfo.UserDgraphInfo.Uuid || dgraphPost.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Only the person who wrote a message can edit it.",
		})
		return
	}
	// A deleted message isn't edited. A database that didn't answer says
	// nothing about the message, so that is a fault the person can retry.
	row, err := business.GetPostByUUID(ctx, postUUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Couldn't check the message just now. Try again in a moment.",
		})
		return
	}
	if row == nil || !row.DeletedAt.IsZero() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "That message was deleted.",
		})
		return
	}

	// get mentions
	mentions, err := helpers.GetMentions(postInfo.HTMLText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdatePost Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	err = business.UpdatePost(ctx, &postInfo, mentionsDgraphUsersList, postUUID, dgraphPost.Channel.Uuid, dgraphPost.PostBy.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdatePost Failed to create post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get Failed to create post",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated post successfully!"})

}

func DeleteCommentInPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToPost

	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentInPost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentUUID, err := uuid.Parse(commentInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentInPost Failed to parse comment UUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse commentUUID",
			"err": err,
		})

		return
	}

	dgraphComment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, commentInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphComment == nil || dgraphComment.Post == nil || dgraphComment.Post.Channel == nil ||
		dgraphComment.CommentBy == nil || !dgraphComment.Post.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentInPost Failed to get comment from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get commentInfo",
			"err": err,
		})

		return
	}

	// The reply's author (still in the channel) or a channel admin. This
	// wrote its 403 and went on to delete the reply anyway.
	if (dgraphComment.CommentBy.Uuid != userInfo.UserDgraphInfo.Uuid || dgraphComment.Post.Channel.IsMember == 0) && dgraphComment.Post.Channel.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized",
		})
		return
	}

	err = business.DeleteCommentOnPost(ctx, commentUUID, dgraphComment, userInfo.UserDgraphInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentInPost Failed to delete comment on post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update post comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "deleted comment on post successfully!",
	})

}

func UpdateCommentInPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToPost

	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentUUID, err := uuid.Parse(commentInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to parse comment UUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse commentUUID",
			"err": err,
		})

		return
	}

	dgraphComment, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, commentInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphComment == nil || dgraphComment.Post == nil || dgraphComment.Post.Channel == nil ||
		dgraphComment.CommentBy == nil || !dgraphComment.Post.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to comment from dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get commentInfo",
			"err": err,
		})

		return
	}

	// Only the reply's author, still in the channel. This wrote its 403 and
	// went on to rewrite the reply anyway, which then showed under the
	// author's name.
	if dgraphComment.CommentBy.Uuid != userInfo.UserDgraphInfo.Uuid || dgraphComment.Post.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized",
		})
		return
	}

	mentions, err := helpers.GetMentions(commentInfo.HTMLText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	err = business.UpdatePostComment(ctx, commentUUID, &commentInfo, mentionsDgraphUsersList, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to update post comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update post comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "updated comment on post successfully!",
	})

}

func CreateCommentInPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToPost

	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddCommentToPost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphPost, err := business.GetDgraphPostByUUID(ctx, commentInfo.PostUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || !dgraphPost.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddCommentToPost Failed to get post info from dgraph err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post info",
			"err": err,
		})
		return
	}

	if dgraphPost.Channel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized",
			"err": err,
		})
		return
	}

	// A deleted message takes no replies.
	if helpers.IsSoftDeleted(dgraphPost.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	mentions, err := helpers.GetMentions(commentInfo.HTMLText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddCommentToPost Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	// "Also send to channel" (Slack parity) also posts the reply as a
	// top-level message in the thread's channel, so people not following the
	// thread still see it. That is a post, so it takes the rules a post does
	// (business/Send): in an announcement channel only its admins may. They
	// are asked before anything is written, so a refusal sends nothing.
	var alsoPost *sendBusiness.ChannelPost
	if commentInfo.AlsoSendToChannel && strings.TrimSpace(commentInfo.HTMLText) != "" {
		alsoPost, err = sendBusiness.PrepareChannelPost(ctx, &userInfo, &adapter.InputCreateOrUpdatePostInfo{
			HTMLText:    commentInfo.HTMLText,
			MediaObj:    commentInfo.MediaObj,
			ChannelUuid: dgraphPost.Channel.Uuid,
		})
		if err != nil {
			writeSendError(w, err)
			return
		}
		// The reply tells the people it mentions; the copy doesn't again.
		alsoPost = alsoPost.WithoutMentions()
	}

	createdCommentData, err := business.CreatePostComment(ctx, &commentInfo, &userInfo, mentionsDgraphUsersList, dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddCommentToPost Failed to create post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create post",
			"err": err,
		})
		return
	}

	// The reply is written, so posting it in the channel too is best-effort:
	// a failure here must not fail the reply, so it is only logged.
	if alsoPost != nil {
		copied, perr := alsoPost.Commit(ctx)
		if perr != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/AddCommentToPost also_send_to_channel failed err: %+v", perr)
		} else if lerr := business.LinkReplyCopy(ctx, createdCommentData.Uuid, copied.Uuid); lerr != nil {
			// The copy is posted; only following the reply's edits is lost.
			helpers.LogErrorWithContext(ctx,
				"controllers/AddCommentToPost Failed to link the reply to its copy err: %+v", lerr)
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "created comment on post successfully!",
		"data": createdCommentData,
	})

}

func PostByUUIDWithAllComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	postId := chi.URLParam(r, "post_uuid")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if len(postId) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Post UUID is empty",
		})
		return
	}

	dgraphPost, err := business.GetDgraphPostByUUIDWithAllComments(ctx, postId, userInfo.UserDgraphInfo.Uid)

	// A message that isn't there is a 404, as a deleted one is, so the page
	// says the thread isn't available rather than that it couldn't load; a
	// graph that doesn't answer says nothing about the message, and is a 503
	// the page can try again.
	if errors.Is(err, postDgraphModels.ErrNotFound) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This thread isn't available."})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/PostByUUIDWithAllComments Failed to get dgraph post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load the thread. Try again."})
		return
	}

	if dgraphPost.Channel.IsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	// A deleted message's thread shows nothing of it: a placeholder over its
	// replies, or no thread when there are none (business.ThreadForMember).
	thread, ok := business.ThreadForMember(dgraphPost)
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got post successfully!",
		"data": thread,
	})

}

func GetOldPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	epochTimeString := chi.URLParam(r, "time_stamp")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	// The post the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	if !readableChannel(w, r, channelUUIDString, userInfo.UserDgraphInfo.Uid) {
		return
	}

	posts, err := business.GetOldPosts(ctx, channelUUIDString, edge)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetOldPost Failed to get posts err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got posts successfully!",
		"data": posts,
	})

}

func GetNewPostsWithCurrentPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	postUUIDString := chi.URLParam(r, "post_uuid")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if _, err := uuid.Parse(channelUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel_uuid must be a uuid"})
		return
	}
	if _, err := uuid.Parse(postUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "post_uuid must be a uuid"})
		return
	}

	channelInfo, err := channelBusiness.GetBasicChannelAndPostInfoByUUID(ctx, channelUUIDString, postUUIDString, userInfo.UserDgraphInfo.Uid)
	if !mayReadChannel(w, r, channelInfo, err) {
		return
	}
	// A message that isn't in this channel, or was deleted, isn't there.
	if channelInfo.Post == nil || helpers.IsSoftDeleted(channelInfo.Post.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't there."})
		return
	}

	posts, err := business.GetDgraphNewPostIncludiongPostFromDgraph(ctx, channelUUIDString, channelInfo.Post.CreatedAt)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewPostsWithCurrentPost Failed to get posts err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got posts successfully!",
		"data": posts,
	})

}

// GetOnlyPostText is a post's text, author and times, for forwarding it: GET
// /po/getOnlyPostText/{channel_uuid}/{post_uuid}. The post is held to the
// channel named (channelBusiness.PostReadableIn): it must be in that channel,
// the channel one the asker can read, and the post not deleted. It used to
// check only that the asker was in the channel named and then read the post
// by its id alone, so anyone in any channel could read any private channel's
// posts, deleted ones too, with a post's id. Every refusal is the same 404, so
// the answer says nothing about a post the asker may not read.
func GetOnlyPostText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	postUUIDString := chi.URLParam(r, "post_uuid")

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel_uuid must be a uuid"})
		return
	}
	if _, err := uuid.Parse(postUUIDString); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "post_uuid must be a uuid"})
		return
	}

	notFound := func() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't there."})
	}

	channelInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if errors.Is(err, channelDgraphModels.ErrNotFound) {
		notFound()
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetOnlyPostText Failed to get channel info err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load that message. Try again."})
		return
	}

	dgraphPost, err := business.GetDgraphPostTextWithChannel(ctx, postUUIDString)
	if errors.Is(err, postDgraphModels.ErrNotFound) {
		notFound()
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetOnlyPostText Failed to get post err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load that message. Try again."})
		return
	}

	if !channelBusiness.PostReadableIn(channelInfo, dgraphPost) {
		notFound()
		return
	}

	// What this answer always held: the channel and the deletion time were
	// read only to check the post against.
	dgraphPost.Channel, dgraphPost.DeletedAt = nil, nil

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got post successfully!",
		"data": dgraphPost,
	})

}

func GetNewPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	epochTimeString := chi.URLParam(r, "time_stamp")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	// The post the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	if !readableChannel(w, r, channelUUIDString, userInfo.UserDgraphInfo.Uid) {
		return
	}

	posts, err := business.GetNewPosts(ctx, channelUUIDString, edge)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewPost Failed to get posts err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got posts successfully!",
		"data": posts,
	})

}

func GetLatestPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if !readableChannel(w, r, channelUUIDString, userInfo.UserDgraphInfo.Uid) {
		return
	}

	posts, err := business.GetLatestPosts(ctx, channelUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPosts Failed to get posts err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got posts successfully!",
		"data": posts,
	})

}

func DeletePost(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var postInfo adapter.InputCreateOrUpdatePostInfo

	err := json.NewDecoder(r.Body).Decode(&postInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphPost, err := business.GetDgraphPostByUUID(ctx, postInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphPost == nil || dgraphPost.Channel == nil || !dgraphPost.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePost Failed to get dgraph post info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post info",
			"err": err,
		})
		return
	}
	// _ = msgTopic
	// _ = typingTopic

	channelUUID, err := uuid.Parse(dgraphPost.Channel.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeletePost Failed to parse channel UUID err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID in req",
			"err": err,
		})
		return
	}

	dgraphChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeletePost Failed to dgraph channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	if dgraphChannel.IsAdmin == 0 && !(userInfo.UserDgraphInfo.Uuid == dgraphPost.PostBy.Uuid) {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.DeletePost(ctx, dgraphPost, channelUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePost Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted post successfully!"})
}

// writeSendError answers a refused send with the status and message the rule
// gives, and anything else as a bad request.
func writeSendError(w http.ResponseWriter, err error) {
	if rj, ok := sendBusiness.AsRejection(err); ok {
		env := helpers.Envolope{"msg": rj.Msg}
		if rj.Err != nil {
			env["err"] = rj.Err
		}
		helpers.WriteJSON(w, rj.Status, env)
		return
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to send", "err": err})
}

// readableChannel answers for a channel's page routes: whether the person
// asking may read the messages of the channel named by raw. When they may
// not, it has written the answer: 400 for an id that isn't one, and
// mayReadChannel's answers. The routes answered an empty 200 for a bad id,
// and a channel whose privacy didn't load made them panic (F-15, F-16).
func readableChannel(w http.ResponseWriter, r *http.Request, raw string, userDgraphUID string) bool {
	channelUUID, err := uuid.Parse(raw)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel_uuid must be a uuid"})
		return false
	}
	channel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(r.Context(), channelUUID, userDgraphUID)
	return mayReadChannel(w, r, channel, err)
}

// mayReadChannel says whether a channel read for the person asking (channel,
// err) lets them read its messages (channelBusiness.CanRead). When it doesn't,
// it has written the answer: 404 for a channel that isn't there, 503 when the
// graph doesn't answer (helpers.WriteReadFailure), 403 for one they can't read.
func mayReadChannel(w http.ResponseWriter, r *http.Request, channel *dgraphStruct.DgraphChannel, err error) bool {
	if err == nil && channel == nil {
		err = channelDgraphModels.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/mayReadChannel")
		return false
	}
	if !channelBusiness.CanRead(channel) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You're not in this private channel."})
		return false
	}
	return true
}
