package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	business "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	messagePipeline "github.com/akashc777/OneCamp/business/MessagePipeline"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func CreateChat(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var chatInfo adapter.ChatInfo

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create chat",
			"err": err,
		})
		return
	}

	// The message pipeline (chat A2): business/Send's rules, once per id the
	// app gave it (Try again after a lost answer is the same message, not a
	// second one), and the write, durable when MESSAGE_PIPELINE switches DMs
	// on, today's otherwise. The route is its thin adapter; a recipient that
	// isn't an id is refused as business/Send refuses it.
	if _, perr := uuid.Parse(chatInfo.ToUuid); perr != nil {
		writeSendError(w, &sendBusiness.Rejection{Status: http.StatusBadRequest, Msg: "Failed to parse toUUID in req", Err: perr})
		return
	}
	written, err := messagePipeline.Messages.Write(ctx, &userInfo, messagePipeline.Write{
		Op: messagePipeline.OpSend,
		To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, chatInfo.ToUuid)},
		Draft: messagePipeline.Draft{
			HTML: chatInfo.TextHtml, Files: chatInfo.MediaObjects, ReplyTo: chatInfo.ReplyToUuid, ClientMsgID: chatInfo.ClientMsgID,
		},
	})
	createdChatInfo := &adapter.OutputCreateChatForChat{Uuid: written.ID, ChatCreated: written.At}
	if rj, ok := sendBusiness.AsRejection(err); ok {
		writeSendError(w, rj)
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChat Failed to create chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create chat",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created chat successfully!", "data": createdChatInfo})
}

func GetGroupDMParticipants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIDString := chi.URLParam(r, "grp_id")
	if grpIDString == "" {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty grp chat uuid in the req",
		})
		return

	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIDString)

	// A failed read is a 503 the app offers Try again for; a 400 read as a
	// group that isn't there (QA F-10). A group that isn't there answers with
	// nothing in it, as a group made on a phone and not yet sent does.
	const where = "controllers/GetGroupDMParticipants"
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "conversation", where)
		return
	}

	if dgraphDm == nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got Dm successfully!", "data": dgraphDm})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to group chat info",
			"err": err,
		})
		return

	}

	notificationType, err := userChatNotificationBusiness.GetNotificationTypeByUserIdAndToUserId(ctx, userInfo.UserDgraphInfo.Uuid, grpIDString)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "conversation", where)
		return
	}

	if len(notificationType) > 0 {
		dgraphDm.NotificationType = notificationType
	}

	dgraphDm.CallActive, err = business.GetChatCallStatus(ctx, grpIDString)

	if err != nil {
		helpers.WriteReadFailure(w, r, err, "conversation", where)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got Dm successfully!", "data": dgraphDm})

}

func CreateGroupChat(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var chatInfo adapter.ChatInfo

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateGroupChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create chat",
			"err": err,
		})
		return
	}

	// The message pipeline, as for a DM: a group's id, or a new group's
	// people, whose id they make.
	written, err := messagePipeline.Messages.Write(ctx, &userInfo, messagePipeline.Write{
		Op:     messagePipeline.OpSend,
		To:     conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: chatInfo.GrpUuid},
		People: chatInfo.Participants,
		Draft: messagePipeline.Draft{
			HTML: chatInfo.TextHtml, Files: chatInfo.MediaObjects, ReplyTo: chatInfo.ReplyToUuid, ClientMsgID: chatInfo.ClientMsgID,
		},
	})
	createdChatInfo := &adapter.OutputCreateChatForChat{Uuid: written.ID, ChatCreated: written.At}
	if rj, ok := sendBusiness.AsRejection(err); ok {
		writeSendError(w, rj)
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateGroupChat Failed to create chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create chat",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created chat successfully!", "data": createdChatInfo})
}

func GetChatText(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	if chatUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	chatDgraphInfo, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatText Failed to get chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info",
			"err": err,
		})
		return
	}

	if chatDgraphInfo == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "Chat message not found",
		})
		return
	}

	// Allow both the sender (From) and recipient (To) to preview the message
	isParticipant := (chatDgraphInfo.From != nil && chatDgraphInfo.From.Uuid == userInfo.UserDgraphInfo.Uuid) ||
		(chatDgraphInfo.To != nil && chatDgraphInfo.To.Uuid == userInfo.UserDgraphInfo.Uuid)

	if !isParticipant {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized to view this message",
		})
		return
	}

	if (chatDgraphInfo.From != nil && chatDgraphInfo.From.DeletedAt != nil && !chatDgraphInfo.From.DeletedAt.IsZero()) ||
		(chatDgraphInfo.To != nil && chatDgraphInfo.To.DeletedAt != nil && !chatDgraphInfo.To.DeletedAt.IsZero()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized to view this message",
		})
		return
	}

	// A deleted message's text is kept in the graph, so it is refused here,
	// as everywhere it would otherwise be read back.
	if helpers.IsSoftDeleted(chatDgraphInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got chat successfully!", "data": chatDgraphInfo})

}

// GetGroupChatText retrieves a group chat message preview for the forward dialog.
// Any current member of the group can preview the message.
func GetGroupChatText(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	if chatUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	chatDgraphInfo, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetGroupChatText Failed to get group chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group chat info",
			"err": err,
		})
		return
	}

	if chatDgraphInfo == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "Group chat message not found",
		})
		return
	}

	// Verify the requesting user is a current member of this group
	if chatDgraphInfo.DM == nil || chatDgraphInfo.DM.GroupingId == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Unable to determine group membership",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, chatDgraphInfo.DM.GroupingId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetGroupChatText Failed to verify group membership err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to verify group membership",
			"err": err,
		})
		return
	}

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized to view this message",
		})
		return
	}

	// A deleted message's text is kept in the graph, so it is refused here,
	// as everywhere it would otherwise be read back.
	if helpers.IsSoftDeleted(chatDgraphInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got group chat successfully!", "data": chatDgraphInfo})

}

func UpdateChatInGroup(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var chatInfo adapter.ChatInfo

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatInGroup Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update chat",
			"err": err,
		})
		return
	}

	chatUUID, err := uuid.Parse(chatInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatInGroup Failed to parse chatUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse toUUID in req",
			"err": err,
		})
		return
	}

	if len(chatInfo.GrpUuid) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "got empty grp id",
		})
		return
	}

	chatDgraphInfo, err := business.GetDgraphChatByUUID(ctx, chatUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatInGroup Failed to get chat postgresInfo err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	if chatDgraphInfo.From == nil || chatDgraphInfo.From.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	// The conversation is the stored message's, never the request's. The
	// request's was used: an author could publish their edit into another
	// group's live stream, and move the message's AI entry there, with that
	// group's id (a hash of its people's ids). Whoever edits must still be
	// in the group, and a deleted message isn't edited.
	if chatDgraphInfo.DM == nil || chatDgraphInfo.DM.GroupingId == "" || chatDgraphInfo.DM.GroupingId != chatInfo.GrpUuid {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't in this conversation."})
		return
	}
	if helpers.IsSoftDeleted(chatDgraphInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}
	group, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, chatDgraphInfo.DM.GroupingId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateChatInGroup Failed to get the group err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't save your edit. Try again."})
		return
	}
	if group == nil || group.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	mentionsDgraphUsersList, err := sendBusiness.Mentions(ctx, &userInfo, sendBusiness.Where{Conversation: chatDgraphInfo.DM.GroupingId}, &chatInfo.TextHtml)
	if err != nil {
		writeSendError(w, err)
		return
	}

	err = business.UpdateChat(ctx, &chatInfo, chatUUID, chatDgraphInfo.DM.GroupingId, chatDgraphInfo.From.Uuid, mentionsDgraphUsersList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatInGroup Failed to update chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update chat",
			"err": err,
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated chat successfully!"})
}

func UpdateChat(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var chatInfo adapter.ChatInfo

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update chat",
			"err": err,
		})
		return
	}

	chatUUID, err := uuid.Parse(chatInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChat Failed to parse chatUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse toUUID in req",
			"err": err,
		})
		return
	}

	chatDgraphInfo, err := business.GetDgraphChatByUUID(ctx, chatUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChat Failed to get chat postgresInfo err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	// A direct message has someone it went to; a group's message is edited
	// through the group's route, which checks the group.
	if chatDgraphInfo == nil || chatDgraphInfo.From == nil || chatDgraphInfo.To == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't in a direct conversation."})
		return
	}
	if chatDgraphInfo.From.Uuid != userInfo.UserDgraphInfo.Uuid || (chatDgraphInfo.To.DeletedAt != nil && !chatDgraphInfo.To.DeletedAt.IsZero()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}
	// A deleted message isn't edited: its text stays in the graph.
	if helpers.IsSoftDeleted(chatDgraphInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	mentionsDgraphUsersList, err := sendBusiness.Mentions(ctx, &userInfo, sendBusiness.Where{People: []string{chatDgraphInfo.From.Uuid, chatDgraphInfo.To.Uuid}}, &chatInfo.TextHtml)
	if err != nil {
		writeSendError(w, err)
		return
	}

	grpId := helpers.GetGroupingId(chatDgraphInfo.To.Uuid, chatDgraphInfo.From.Uuid)

	err = business.UpdateChat(ctx, &chatInfo, chatUUID, grpId, chatDgraphInfo.From.Uuid, mentionsDgraphUsersList)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChat Failed to update chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update chat",
			"err": err,
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated chat successfully!"})
}

func DeleteChat(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var chatInfo adapter.ChatInfo

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	chatUUID, err := uuid.Parse(chatInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChat Failed to parse chatUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse toUUID in req",
			"err": err,
		})
		return
	}

	chatDgraphInfo, err := business.GetDgraphChatBasicByUUID(ctx, chatInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChat Failed to get chat chatDgraphInfo err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	if chatDgraphInfo.From.Uuid != userInfo.UserDgraphInfo.Uuid || !chatDgraphInfo.DeletedAt.IsZero() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	err = business.DeleteChat(ctx, &chatInfo, chatUUID, chatDgraphInfo.DM.GroupingId, chatDgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentOnPost Failed to create chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create comment",
			"err": err,
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted chat successfully!"})
}

func GetUserListWithLatestChatWithUserIdAndSearchText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var dmSearchInfo adapter.InputDmSearchText

	err := json.NewDecoder(r.Body).Decode(&dmSearchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserListWithLatestChatWithUserIdAndSearchText Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	searchText := dmSearchInfo.SearchText

	if len(searchText) == 0 {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got Empty Search search string",
		})
		return
	}

	// Sanitise before passing to Dgraph regexp filter.
	safeSearch, sErr := dgraphquery.SanitizeSearchTerm(searchText)
	if sErr != nil {
		if errors.Is(sErr, dgraphquery.ErrEmpty) {
			helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "empty search", "data": nil})
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search text is too long or invalid"})
		return
	}

	usersDmList, err := business.GetUserListWithLatestChatWithUserIdAndSearchText(ctx, userInfo.UserDgraphInfo.Uuid, safeSearch)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserListWithLatestChatWithUserIdAndSearchText Failed to get users list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got users list successfully!", "data": usersDmList})

}

func GetUserChatListWithLatestChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphUser, err := business.GetUserChatListWithLatestChat(ctx, userInfo.UserPostgresInfo.Id)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserListWithLatestChatWithUserIdAndSearchText Failed to get user's chat list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's chat list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": dgraphUser})

}

func GetOldGroupChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")
	epochTimeString := chi.URLParam(r, "time_stamp")

	// The message the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	if len(grpIdString) == 0 {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse grpId string",
			"err": err,
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetOldGroupChats Failed to get grp chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get grp chat info",
			"err": err,
		})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	chats, err := business.GetDgraphOldGroupChatFromDgraph(ctx, edge, grpIdString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetOldGroupChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetOldChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	secondUserUUIDString := chi.URLParam(r, "user_uuid")
	epochTimeString := chi.URLParam(r, "time_stamp")

	// The message the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	secondUserUUID, err := uuid.Parse(secondUserUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/getOldChats Failed to parse userUUID string err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to pas userUUID string",
			"err": err,
		})
		return
	}

	secondUserInfo, err := userBusiness.GetUserByUUID(ctx, secondUserUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/getOldChats Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if secondUserInfo == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/getOldChats Failed to get user")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	chats, err := business.GetDgraphOldChatFromDgraph(ctx, edge, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/getOldChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetNewGroupChatsIncludingChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	if chatUUIDString == "" || grpIdString == "" {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse chatUUID ot grpId string",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString)

	if err != nil || dgraphDm == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewGroupChatsIncludingChat Failed to get group chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group chat",
			"err": err,
		})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	dgraphChatInfo, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewGroupChatsIncludingChat Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	chats, err := business.GetDgraphNewGroupChatIncludingChatFromDgraph(ctx, *dgraphChatInfo.CreatedAt, grpIdString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewGroupChatsIncludingChat Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetNewChatsIncludingChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	secondUserUUIDString := chi.URLParam(r, "user_uuid")

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	if chatUUIDString == "" || secondUserUUIDString == "" {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse chatUUID string",
		})
		return
	}

	dgraphChatInfo, err := business.GetDgraphChatOnlyTextByUUID(ctx, chatUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChatsIncludingChat Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	chats, err := business.GetDgraphNewChatIncludingChatFromDgraph(ctx, *dgraphChatInfo.CreatedAt, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetNewGroupChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")
	epochTimeString := chi.URLParam(r, "time_stamp")

	// The message the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString)

	if err != nil || dgraphDm == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewGroupChats Failed to get grp chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get grp chat info",
			"err": err,
		})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	chats, err := business.GetDgraphNewGroupChatFromDgraph(ctx, edge, grpIdString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewGroupChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetNewChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	secondUserUUIDString := chi.URLParam(r, "user_uuid")
	epochTimeString := chi.URLParam(r, "time_stamp")

	// The message the page starts next to (helpers.PageEdge).
	edge, err := helpers.PageEdgeOf(r, epochTimeString)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	secondUserUUID, err := uuid.Parse(secondUserUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChats Failed to parse userUUID string err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to pas userUUID string",
			"err": err,
		})
		return
	}

	secondUserInfo, err := userBusiness.GetUserByUUID(ctx, secondUserUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChats Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if secondUserInfo == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChats Failed to get user")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	chats, err := business.GetDgraphNewChatFromDgraph(ctx, edge, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetNewChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetLatestGroupChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")

	if len(grpIdString) == 0 {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse grpId string",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString)

	if err != nil || dgraphDm == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestGroupChats Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	chats, err := business.GetDgraphLatestGroupChatFromDgraph(ctx, userInfo.UserPostgresInfo.Id, grpIdString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestGroupChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func GetLatestChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	secondUserUUIDString := chi.URLParam(r, "user_uuid")

	secondUserUUID, err := uuid.Parse(secondUserUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChats Failed to parse userUUID string err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to pas userUUID string",
			"err": err,
		})
		return
	}

	secondUserInfo, err := userBusiness.GetUserByUUID(ctx, secondUserUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChats Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if secondUserInfo == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChats Failed to get user")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	chats, err := business.GetDgraphLatestChatFromDgraph(ctx, userInfo.UserPostgresInfo.Id, secondUserUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChats Failed to get chats info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chat list successfully!", "data": chats})

}

func CreateOrUpdateChatReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var reactionInfo adapter.InputUpdateReactionForChat

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, reactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphChat == nil || dgraphChat.DM.ParticipantIsMember == 0 || (dgraphChat.From.DeletedAt != nil && !dgraphChat.From.DeletedAt.IsZero()) || (dgraphChat.To != nil && dgraphChat.To.DeletedAt != nil && !dgraphChat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatReaction Failed to get dgraphChat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info",
			"err": err,
		})
		return
	}

	// A uid changes that reaction, and only the caller's own on this message.
	if reactionInfo.ReactionDgraphUid != "" {
		dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnChat, dgraphChat.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
		if refusal != nil {
			helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
			return
		}
		reactionInfo.ReactionDgraphUid = dgraphReaction.Uid
	}

	// A deleted message takes no reactions.
	if helpers.IsSoftDeleted(dgraphChat.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	reactionUID, err := business.CreateOrUpdateChatReaction(ctx, &reactionInfo, dgraphChat, &userInfo.UserDgraphInfo, dgraphChat.DM.GroupingId)

	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't add the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	reactionInfo.ReactionDgraphUid = reactionUID

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created/updated reaction successfully!", "data": reactionInfo})
}

func DeleteChatReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var reactionInfo adapter.InputUpdateReactionForChat

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChatReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, reactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphChat == nil || dgraphChat.DM.ParticipantIsMember == 0 || (dgraphChat.From.DeletedAt != nil && !dgraphChat.From.DeletedAt.IsZero()) || (dgraphChat.To != nil && dgraphChat.To.DeletedAt != nil && !dgraphChat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChatReaction Failed to get chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info",
			"err": err,
		})
		return
	}

	dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnChat, dgraphChat.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
	if refusal != nil {
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
		return
	}

	err = business.DeleteChatReaction(ctx, reactionInfo.Uuid, dgraphReaction.Uid, &userInfo.UserDgraphInfo, dgraphChat.DM.GroupingId)
	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't remove the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "removed reaction successfully!"})
}

func GetDgraphChatByUUIDWithAllComments(w http.ResponseWriter, r *http.Request) {
	chatId := chi.URLParam(r, "chat_uuid")
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, chatId, userInfo.UserDgraphInfo.Uid)

	// A message that isn't there, or isn't in a conversation, is a 404, as a
	// deleted one is; a graph that doesn't answer is a 503 the page can try
	// again (helpers.WriteReadFailure).
	if err == nil && (dgraphChat == nil || dgraphChat.DM == nil) {
		err = chatDgraphModels.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "thread", "controllers/GetDgraphChatByUUIDWithAllComments")
		return
	}

	if dgraphChat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You're not in this conversation."})
		return
	}

	// The thread itself, answered the same way: a failed read was a 400 that
	// said the request was wrong.
	dgraphChatWithAllComments, err := business.GetDgraphChatByUUIDWithAllComments(ctx, chatId)
	if err == nil && dgraphChatWithAllComments == nil {
		err = chatDgraphModels.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "thread", "controllers/GetDgraphChatByUUIDWithAllComments")
		return
	}

	// A deleted message's thread shows nothing of it: a placeholder over its
	// replies, or no thread when there are none (business.ChatThreadForMember).
	thread, ok := business.ChatThreadForMember(dgraphChatWithAllComments)
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got reaction comment successfully!", "data": thread})

}

func CreateChatComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToChat
	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChatReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, commentInfo.ChatUuid, userInfo.UserDgraphInfo.Uid)

	// A message that isn't there, or isn't in a conversation, takes no reply:
	// 404, never a panic on what the lookup didn't find (F-15).
	if err == nil && (dgraphChat == nil || dgraphChat.DM == nil || dgraphChat.From == nil) {
		err = chatDgraphModels.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "message", "controllers/CreateChatComment")
		return
	}
	// Someone in it has left: trying again won't help, so it never says to.
	if helpers.IsSoftDeleted(dgraphChat.From.DeletedAt) || (dgraphChat.To != nil && helpers.IsSoftDeleted(dgraphChat.To.DeletedAt)) {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Someone in this conversation has left the workspace, so it takes no new replies."})
		return
	}

	if dgraphChat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You're not in this conversation."})
		return
	}

	// A deleted message takes no replies.
	if helpers.IsSoftDeleted(dgraphChat.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}

	mentionsDgraphUsersList, err := sendBusiness.Mentions(ctx, &userInfo, sendBusiness.Where{Thread: true, Conversation: dgraphChat.DM.GroupingId}, &commentInfo.HTMLText)
	if err != nil {
		writeSendError(w, err)
		return
	}

	createdComment, err := business.CreateChatComment(ctx, &commentInfo, &userInfo, mentionsDgraphUsersList, dgraphChat.DM.GroupingId, dgraphChat.From.Uid, dgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChatComment Failed to create chat comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to create chat comment",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created chat comment successfully!", "data": createdComment})

}

func UpdateChatComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToChat
	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to parse the body of the req err: %+v",
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

	dgraphComment, err := commentBusiness.GetDgraphCommentInfoByUUID(ctx, commentInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateCommentInPost Failed to get comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get comment info",
			"err": err,
		})

		return
	}

	if dgraphComment.CommentBy.Uid != userInfo.UserDgraphInfo.Uid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})

		return
	}

	// The reply's conversation is the one it is in, never the request's: the
	// request's was used, unchecked, so its author could publish the edit
	// into another conversation's live stream. Whoever edits must be in the
	// conversation, and neither a deleted reply nor one under a deleted
	// message is edited.
	stored, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, commentInfo.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil || stored == nil || stored.Chat == nil || stored.Chat.Uuid == "" ||
		(commentInfo.ChatUuid != "" && commentInfo.ChatUuid != stored.Chat.Uuid) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That reply isn't in this conversation."})
		return
	}
	if stored.Chat.DM == nil || stored.Chat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}
	if helpers.IsSoftDeleted(stored.DeletedAt) || helpers.IsSoftDeleted(stored.Chat.DeletedAt) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message was deleted."})
		return
	}
	commentInfo.ChatUuid = stored.Chat.Uuid

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, commentInfo.ChatUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || (dgraphChat.From.DeletedAt != nil && !dgraphChat.From.DeletedAt.IsZero()) || (dgraphChat.To != nil && dgraphChat.To.DeletedAt != nil && !dgraphChat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to get dgraph chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't save your edit. Try again.",
			"err": err,
		})
		return
	}

	mentionsDgraphUsersList, err := sendBusiness.Mentions(ctx, &userInfo, sendBusiness.Where{Thread: true, Conversation: stored.Chat.DM.GroupingId}, &commentInfo.HTMLText)
	if err != nil {
		writeSendError(w, err)
		return
	}

	err = business.UpdateChatComment(ctx, commentUUID, &commentInfo, mentionsDgraphUsersList, dgraphChat, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to update comment in dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't save your edit. Try again.",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated chat comment successfully!"})

}

func DeleteCommentOnChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var commentInfo adapter.InputCreateOrUpdateCommentToChat
	err := json.NewDecoder(r.Body).Decode(&commentInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentOnChat Failed to parse the body of the req err: %+v",
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
			"controllers/DeleteCommentOnChat Failed to parse comment UUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse commentUUID",
			"err": err,
		})

		return
	}

	dgraphComment, err := commentBusiness.GetDgraphCommentInfoByUUID(ctx, commentInfo.Uuid)

	if err != nil || (dgraphComment.Chat.From.DeletedAt != nil && !dgraphComment.Chat.From.DeletedAt.IsZero()) || (dgraphComment.Chat.To != nil && dgraphComment.Chat.To.DeletedAt != nil && !dgraphComment.Chat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentOnChat Failed to get comment info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get comment info",
			"err": err,
		})

		return
	}

	if dgraphComment.CommentBy.Uid != userInfo.UserDgraphInfo.Uid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})

		return
	}

	err = business.DeleteCommentOnChat(ctx, commentUUID, userInfo.UserDgraphInfo.Uuid, dgraphComment.Chat.Uuid, dgraphComment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteCommentOnChat Failed delete comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to delete comment",
			"err": err,
		})

		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted chat comment successfully!"})
}

func CreateOrUpdateChatCommentReaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var reactionInfo adapter.InputUpdateReactionForCommentInChat

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatCommentReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	dgraphComment, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, reactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphComment == nil || (dgraphComment.Chat.From.DeletedAt != nil && !dgraphComment.Chat.From.DeletedAt.IsZero()) || (dgraphComment.Chat.To != nil && dgraphComment.Chat.To.DeletedAt != nil && !dgraphComment.Chat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatCommentReaction Failed to dgraph comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	if dgraphComment.Chat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	// A uid changes that reaction, and only the caller's own on this comment.
	if reactionInfo.ReactionDgraphUid != "" {
		dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, dgraphComment.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
		if refusal != nil {
			helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
			return
		}
		reactionInfo.ReactionDgraphUid = dgraphReaction.Uid
	}

	// The answer is the reaction: the one added, or the one the person already had.
	created, err := business.CreateOrUpdateChatCommentReaction(ctx, &reactionInfo, dgraphComment, &userInfo.UserDgraphInfo)
	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't add the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}
	reactionInfo.ReactionDgraphUid = created

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "created/updated chat comment successfully!", "data": reactionInfo})

}

func DeleteReactionOnCommentChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var reactionInfo adapter.InputUpdateReactionForCommentInChat

	err := json.NewDecoder(r.Body).Decode(&reactionInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	commentDgraphInfo, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, reactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || commentDgraphInfo == nil || commentDgraphInfo.Chat.DM.ParticipantIsMember == 0 || (commentDgraphInfo.Chat.From.DeletedAt != nil && !commentDgraphInfo.Chat.From.DeletedAt.IsZero()) || (commentDgraphInfo.Chat.To != nil && commentDgraphInfo.Chat.To.DeletedAt != nil && !commentDgraphInfo.Chat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentChat Failed to get comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't remove the reaction. Try again.",
			"err": err,
		})
		return
	}

	dgraphReaction, refusal := reactionBusiness.MayChange(ctx, reactionBusiness.OnComment, commentDgraphInfo.Uid, reactionInfo.ReactionDgraphUid, userInfo.UserDgraphInfo.Uid)
	if refusal != nil {
		helpers.WriteJSON(w, refusal.Status, helpers.Envolope{"msg": refusal.Msg})
		return
	}

	err = business.DeleteReactionOnCommentChat(ctx, commentDgraphInfo, dgraphReaction.Uid, &userInfo.UserDgraphInfo)

	if err != nil {
		status, msg := reactionBusiness.Answer(err, "Couldn't remove the reaction. Try again.")
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "removed chat comment successfully!"})
}

func PublishTypingInChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var typingInfo adapter.InputPublishTypingInChat

	err := json.NewDecoder(r.Body).Decode(&typingInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/PublishTypingInChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	if len(typingInfo.GrpUuid) == 0 {
		if len(typingInfo.UserUuid) == 0 {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Got empty grpId or userId",
			})
			return
		}

		typingInfo.GrpUuid = helpers.GetGroupingId(typingInfo.UserUuid, userInfo.UserDgraphInfo.Uuid)

	}

	// One of its people: a group's id is a hash anyone can work out from
	// its people, and this said "X is typing" into any group it named.
	if !conversationBusiness.MayType(ctx, &userInfo, conversationBusiness.ChatRef(typingInfo.GrpUuid)) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	business.PublishChatTyping(&userInfo.UserDgraphInfo, typingInfo.GrpUuid)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "published chat typing successfully!"})

}

func MakeVideoCallForGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoCallInfo adapter.InputMakeVideoChatCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForGroup Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if makeVideoCallInfo.GrpUuid == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat grp Id is empty",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, makeVideoCallInfo.GrpUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForGroup Failed to get group info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group info",
			"err": err,
		})
		return
	}

	// Said in the person's words, as a channel's huddle says it (QA F-22).
	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Only people in the group can join its call.",
		})
		return
	}

	var uerToken adapter.UserTokenOutput

	uerToken.TokenString, uerToken.AlreadyExisted, err = business.MakeVideoCall(ctx, makeVideoCallInfo.GrpUuid, &userInfo.UserDgraphInfo, false, makeVideoCallInfo.Language, true)

	if err != nil {

		// Put out of it by a host, or it is locked: said in its own words.
		if helpers.WriteCallRefusal(w, err) {
			return
		}

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForGroup Failed to make video call err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to make video call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got token successfully!", "data": uerToken})

}

func StartVideoCallRecordingForGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoCallInfo adapter.InputMakeVideoChatCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForGroup Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if makeVideoCallInfo.GrpUuid == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat grp Id is empty",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, makeVideoCallInfo.GrpUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForGroup Failed to get group info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group info",
			"err": err,
		})
		return
	}

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.StartRecordingChatCall(ctx, &userInfo.UserDgraphInfo, makeVideoCallInfo.GrpUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForGroup Failed to start video call recording err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to start video call recording",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "started call recording successfully!"})

}

func StopVideoCallRecordingForGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoCallInfo adapter.InputMakeVideoChatCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForGroup Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if makeVideoCallInfo.GrpUuid == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat grp Id is empty",
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, makeVideoCallInfo.GrpUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForGroup Failed to get group info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group info",
			"err": err,
		})
		return
	}

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.StopRecordingChatCall(ctx, makeVideoCallInfo.GrpUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForGroup Failed to stop video call recording err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to stop video call recording",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "stopped call recording successfully!"})

}

func MakeVideoCallForChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoCallInfo adapter.InputMakeVideoChatCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	userUUIDString := makeVideoCallInfo.UserUuid

	if userUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	dgraphUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForChat Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if dgraphUser == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed user does not exist",
		})
		return
	}

	grpId := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, userUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForChat Failed to get grp id info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	var uerToken adapter.UserTokenOutput

	uerToken.TokenString, uerToken.AlreadyExisted, err = business.MakeVideoCall(ctx, grpId, &userInfo.UserDgraphInfo, false, makeVideoCallInfo.Language, false)

	if err != nil {

		// Put out of it by a host, or it is locked: said in its own words.
		if helpers.WriteCallRefusal(w, err) {
			return
		}

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoCallForChat Failed to make video call err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to make video call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got token successfully!", "data": uerToken})

}

func StartVideoCallRecordingForChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var startVideoCallRecordingInfo adapter.InputStartVideoChatCallRecording

	err := json.NewDecoder(r.Body).Decode(&startVideoCallRecordingInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	userUUIDString := startVideoCallRecordingInfo.UserUuid

	if userUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	dgraphUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForChat Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if dgraphUser == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed user does not exist",
		})
		return
	}

	actualGrpId := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, userUUIDString)

	err = business.StartRecordingChatCall(ctx, &userInfo.UserDgraphInfo, actualGrpId)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoCallRecordingForChat Failed to record call err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to record call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "recording started successfully!"})

}

func StopVideoCallRecordingForChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var startVideoCallRecordingInfo adapter.InputStartVideoChatCallRecording

	err := json.NewDecoder(r.Body).Decode(&startVideoCallRecordingInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	userUUIDString := startVideoCallRecordingInfo.UserUuid

	if userUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	dgraphUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForChat Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	if dgraphUser == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed user does not exist",
		})
		return
	}

	grpId := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, userUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForChat Failed to get grp id info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	err = business.StopRecordingChatCall(ctx, grpId)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoCallRecordingForChat Failed to stop record call err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to record call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "recording stoped successfully!"})

}

func GetChatRecordingList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	if chatUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat uuid is empty",
		})
		return
	}

	queryParams := r.URL.Query()
	startDate := queryParams.Get("startDate")
	endDate := queryParams.Get("endDate")

	pageValue := queryParams["pageIndex"]
	pageIndex := 0
	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 100)

	if len(pageValue) > 0 {
		pageInt, err := strconv.Atoi(pageValue[0])
		if err == nil {
			pageIndex = pageInt
		}
	}

	grpIdString := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, chatUUIDString)

	recordingsPagination, err := business.GetDgraphDmRecordingListFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, startDate, endDate, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingList Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the conversation's recordings.",
			"err": err,
		})
		return
	}

	if recordingsPagination.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got chat recording list successfully!", "data": recordingsPagination})

}

func GetGrpChatRecordingList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")

	if grpIdString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "grp id is empty",
		})
		return
	}

	queryParams := r.URL.Query()
	startDate := queryParams.Get("startDate")
	endDate := queryParams.Get("endDate")

	pageValue := queryParams["pageIndex"]
	pageIndex := 0
	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 100)

	if len(pageValue) > 0 {
		pageInt, err := strconv.Atoi(pageValue[0])
		if err == nil {
			pageIndex = pageInt
		}
	}

	recordingsPagination, err := business.GetDgraphDmRecordingListFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, startDate, endDate, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingList Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the group's recordings.",
			"err": err,
		})
		return
	}

	if recordingsPagination.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got grp recording list successfully!", "data": recordingsPagination})

}

func GetChatRecordingTranscript(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	queryParams := r.URL.Query()
	pageIndexStr := queryParams["pageIndex"]
	egressIdString := chi.URLParam(r, "egress_id")

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	var err error

	if !queryParams.Has("pageSize") || len(pageIndexStr) == 0 || len(chatUUIDString) == 0 || len(egressIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	grpIdString := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, chatUUIDString)

	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 100)

	pageIndex := 0
	if len(pageIndexStr) != 0 {
		pageIndex, err = strconv.Atoi(pageIndexStr[0])
		if err != nil {
			if err != nil {

				helpers.LogErrorWithContext(ctx,
					"controllers/GetChatRecordingTranscript Failed to parse pageIndex query param to int err: %+v",
					err)

				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "Failed to parse filters query param",
					"err": err,
				})
				return

			}
		}
	}

	dgraphDm, err := business.GetDMRecordingTranscript(ctx, grpIdString, userInfo.UserDgraphInfo.Uid, egressIdString, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingTranscript Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the recording's transcript.",
			"err": err,
		})
		return
	}

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 || len(dgraphDm.Recordings) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphDm.Recordings[0].TranscriptCount + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got dm info successfully!",
		"data":      dgraphDm.Recordings[0],
		"pageCount": pageCount,
	})

}

func GetGrpChatRecordingTranscript(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	queryParams := r.URL.Query()
	pageIndexStr := queryParams["pageIndex"]
	egressIdString := chi.URLParam(r, "egress_id")

	grpIdString := chi.URLParam(r, "grp_id")

	var err error

	if !queryParams.Has("pageSize") || len(pageIndexStr) == 0 || len(grpIdString) == 0 || len(egressIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	pageSize := helpers.SizeParam(queryParams, "pageSize", 20, 100)

	pageIndex := 0
	if len(pageIndexStr) != 0 {
		pageIndex, err = strconv.Atoi(pageIndexStr[0])
		if err != nil {
			if err != nil {

				helpers.LogErrorWithContext(ctx,
					"controllers/GetGrpChatRecordingTranscript Failed to parse pageIndex query param to int err: %+v",
					err)

				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "Failed to parse filters query param",
					"err": err,
				})
				return

			}
		}
	}

	dgraphDm, err := business.GetDMRecordingTranscript(ctx, grpIdString, userInfo.UserDgraphInfo.Uid, egressIdString, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingTranscript Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Couldn't load the recording's transcript.",
			"err": err,
		})
		return
	}

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 || len(dgraphDm.Recordings) == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphDm.Recordings[0].TranscriptCount + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got dm info successfully!",
		"data":      dgraphDm.Recordings[0],
		"pageCount": pageCount,
	})

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
