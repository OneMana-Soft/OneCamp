package controllers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	business "github.com/akashc777/OneCamp/business/Post"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
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
	// Every rule for posting lives in business/Send, shared with scheduled
	// messages, so a post sent now and one sent later obey the same checks.
	post, err := sendBusiness.PrepareChannelPost(ctx, &userInfo, &postInfo)
	if err != nil {
		writeSendError(w, err)
		return
	}
	cratedPostInfo, err := post.Commit(ctx)
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

	if err != nil || !commentInfo.Post.Channel.DeletedAt.IsZero() {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostCommentReaction Failed to get dgraph comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get dgraph comment info",
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

	reactionDgraph, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, commentReactionInfo.ReactionDgraphUid)

	if reactionDgraph.AddedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	err = business.DeleteReactionOnCommentPost(ctx, commentInfo, commentReactionInfo.ReactionDgraphUid)

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
			"msg": " Failed to get post info from dgraph",
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

	reactionDgraphInfo, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, postReactionInfo.ReactionDgraphUid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeletePostReaction Failed to reactionInfo  err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete reaction",
			"err": err,
		})
		return
	}

	if reactionDgraphInfo.AddedBy.Uid != userInfo.UserDgraphInfo.Uid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
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

	commentPostReactionInfo.ReactionDgraphUid, err = business.CreateOrUpdatePostCommentReaction(ctx, &commentPostReactionInfo, dgraphComment, &userInfo.UserDgraphInfo)

	if err != nil {
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
			"msg": "Failed to get dgraph post",
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
		if _, perr := alsoPost.Commit(ctx); perr != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/AddCommentToPost also_send_to_channel failed err: %+v", perr)
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

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/PostByUUIDWithAllComments Failed to get dgraph post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get post",
			"err": err,
		})
		return
	}

	if dgraphPost.Channel.IsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "got post successfully!",
		"data": dgraphPost,
	})

}

func GetOldPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	epochTimeString := chi.URLParam(r, "time_stamp")
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "time_stamp must be a whole number of seconds"})
		return
	}

	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetOldPost Failed to parse chat UUID err: %+v",
			err)
		return
	}

	lastPostTime := time.Unix(epochTime, 0)

	channelInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetOldPost Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	if channelInfo.IsMember == 0 && *channelInfo.IsPrivate {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post since user is not member of private channel",
			"err": err,
		})
		return
	}

	posts, err := business.GetOldPosts(ctx, channelUUIDString, lastPostTime)

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

	_, err := uuid.Parse(postUUIDString)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/GetNewPostsWithCurrentPost Failed to parse post uuid err: %+v",
			err)
		return

	}

	_, err = uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNewPostsWithCurrentPost Failed to parse channel UUID err: %+v",
			err)
		return
	}

	channelInfo, err := channelBusiness.GetBasicChannelAndPostInfoByUUID(ctx, channelUUIDString, postUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil || channelInfo.Post == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewPostsWithCurrentPost Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	if (channelInfo.IsMember == 0 && *channelInfo.IsPrivate) || (!channelInfo.Post.DeletedAt.IsZero()) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post since user is not member of private channel",
		})
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

func GetOnlyPostText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	postUUIDString := chi.URLParam(r, "post_uuid")

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNewPost Failed to parse channel UUID err: %+v",
			err)
		return
	}

	channelInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetOnlyPostText Failed to parse time stamp err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	if channelInfo.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	dgraphPost, err := business.GetDgraphPostOnlyTextDgraph(ctx, postUUIDString)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/GetOnlyPostText Failed to post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post",
			"err": err,
		})
		return
	}

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

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"business/GetNewPost Failed to parse time stamp err: %+v",
			err)
		return

	}

	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNewPost Failed to parse channel UUID err: %+v",
			err)
		return
	}

	lastPostTime := time.Unix(epochTime, 0)

	channelInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewPost Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	if channelInfo.IsMember == 0 && *channelInfo.IsPrivate {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post since user is not member of private channel",
			"err": err,
		})
		return
	}

	posts, err := business.GetNewPosts(ctx, channelUUIDString, lastPostTime)

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

	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestPosts Failed to parse channel UUID err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID",
		})
		return
	}

	channelInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPosts Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts",
			"err": err,
		})
		return
	}

	if channelInfo.IsMember == 0 && *channelInfo.IsPrivate {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post since user is not member of private channel",
			"err": err,
		})
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
