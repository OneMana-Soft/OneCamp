package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	business "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	lastSeenChatBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	reactionBusiness "github.com/akashc777/OneCamp/business/Reaction"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
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

	// Every rule for messaging lives in business/Send, shared with scheduled
	// messages, so a message sent now and one sent later obey the same checks.
	dm, err := sendBusiness.PrepareDirectMessage(ctx, &userInfo, &chatInfo)
	if err != nil {
		writeSendError(w, err)
		return
	}
	createdChatInfo, err := dm.Commit(ctx)
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

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateGroupChat Failed to get dm info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to group chat info",
			"err": err,
		})
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
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProfileByUserId Failed to get user's notification type err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's notification type err",
			"err": err,
		})
		return
	}

	if len(notificationType) > 0 {
		dgraphDm.NotificationType = notificationType
	}

	dgraphDm.CallActive, err = business.GetChatCallStatus(ctx, grpIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProfileByUserId Failed to get dm call status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dm call status err",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got Dm successfully!", "data": dgraphDm})

}

func AddParticipantToGroupChat(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var chatInfo adapter.InputAddParticipantToGroupChat

	err := json.NewDecoder(r.Body).Decode(&chatInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse payload",
			"err": err,
		})
		return
	}

	dgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, chatInfo.GrpUuid)

	if err != nil || dgraphDm == nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to get group chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group chat info",
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

	newDgraphUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, chatInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	var userUUIDList []string

	userUUIDList = append(userUUIDList, newDgraphUser.Uuid)

	for _, p := range dgraphDm.Participants {
		userUUIDList = append(userUUIDList, p.Uuid)
	}

	newGrpId, err := helpers.GenerateGroupID(userUUIDList)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info",
			"err": err,
		})
		return
	}

	newDgraphDm, err := business.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, newGrpId)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to get group chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get group chat info",
			"err": err,
		})
		return
	}

	if newDgraphDm != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"msg":  "Group with these participants already exists",
			"data": helpers.Envolope{"new_grp_id": newGrpId},
		})
		return

	}

	err = business.AddParticipantToGroupChat(ctx, chatInfo.GrpUuid, newGrpId, newDgraphUser.Uid, newDgraphUser.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddParticipantToGroupChat Failed to add participant to group chat err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add participant to group chat",
			"err": err,
		})
		return
	}

	// Initialize tracking for the newly added participant (plus any existing members who missed initialization)
	go func() {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = lastSeenChatBusiness.BulkCreateLastSeenChatIfNotExists(timeoutCtx, userUUIDList, newGrpId)
	}()

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "added participant to group chat successfully!", "data": helpers.Envolope{"new_grp_id": newGrpId}})
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

	group, err := sendBusiness.PrepareGroupMessage(ctx, &userInfo, &chatInfo)
	if err != nil {
		writeSendError(w, err)
		return
	}
	createdChatInfo, err := group.Commit(ctx)
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

	if chatDgraphInfo.From.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	mentions, err := helpers.GetMentions(chatInfo.TextHtml)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatInGroup Failed to get chat mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat mentions",
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

	err = business.UpdateChat(ctx, &chatInfo, chatUUID, chatInfo.GrpUuid, chatDgraphInfo.From.Uuid, mentionsDgraphUsersList)
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

	if chatDgraphInfo.From.Uuid != userInfo.UserDgraphInfo.Uuid || (chatDgraphInfo != nil && chatDgraphInfo.To.DeletedAt != nil && !chatDgraphInfo.To.DeletedAt.IsZero()) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Failed to get chat info req",
			"err": err,
		})
		return
	}

	mentions, err := helpers.GetMentions(chatInfo.TextHtml)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreUpdateChatateChat Failed to get chat mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat mentions",
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

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse epoch time",
			"err": err,
		})
		return
	}

	if len(grpIdString) == 0 {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse grpId string",
			"err": err,
		})
		return
	}

	lastChatTime := time.Unix(epochTime, 0)

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

	chats, err := business.GetDgraphOldGroupChatFromDgraph(ctx, lastChatTime, grpIdString)

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

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "time_stamp must be a whole number of seconds"})
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

	lastChatTime := time.Unix(epochTime, 0)

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

	chats, err := business.GetDgraphOldChatFromDgraph(ctx, lastChatTime, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString)

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

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "time_stamp must be a whole number of seconds"})
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

	lastChatTime := time.Unix(epochTime, 0)

	chats, err := business.GetDgraphNewGroupChatFromDgraph(ctx, lastChatTime, grpIdString)

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

	epochTime, err := strconv.ParseInt(epochTimeString, 10, 64)
	if err != nil {
		// A malformed time in the URL is the caller's mistake: say so, don't panic.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "time_stamp must be a whole number of seconds"})
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

	lastChatTime := time.Unix(epochTime, 0)

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

	chats, err := business.GetDgraphNewChatFromDgraph(ctx, lastChatTime, userInfo.UserDgraphInfo.Uuid, secondUserUUIDString)

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

	reactionUID, err := business.CreateOrUpdateChatReaction(ctx, &reactionInfo, dgraphChat, &userInfo.UserDgraphInfo, dgraphChat.DM.GroupingId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatReaction Failed to get dgraphChat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info",
			"err": err,
		})
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

	dgraphReaction, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, reactionInfo.ReactionDgraphUid)
	if err != nil || dgraphReaction == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChatReaction Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	if dgraphReaction.AddedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Unauthorized",
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

	err = business.DeleteChatReaction(ctx, dgraphChat.Uid, dgraphReaction.Uid, &userInfo.UserDgraphInfo, dgraphChat.DM.GroupingId, reactionInfo.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteChatReaction Failed to remove chat reaction err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove chat reaction",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "removed reaction successfully!"})
}

func GetDgraphChatByUUIDWithAllComments(w http.ResponseWriter, r *http.Request) {
	chatId := chi.URLParam(r, "chat_uuid")
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, chatId, userInfo.UserDgraphInfo.Uid)

	if err != nil || dgraphChat == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphChatByUUIDWithAllComments Failed to get chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat info",
			"err": err,
		})
		return
	}

	if dgraphChat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized",
		})
		return
	}

	dgraphChatWithAllComments, err := business.GetDgraphChatByUUIDWithAllComments(ctx, chatId)
	if err != nil || dgraphChatWithAllComments == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphChatByUUIDWithAllComments Failed to get chat and all comments err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got reaction comment successfully!", "data": dgraphChatWithAllComments})

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

	if err != nil || (dgraphChat.From.DeletedAt != nil && !dgraphChat.From.DeletedAt.IsZero()) || (dgraphChat.To != nil && dgraphChat.To.DeletedAt != nil && !dgraphChat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChatComment Failed to get dgraph chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph chat info",
			"err": err,
		})
		return
	}

	if dgraphChat.DM.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorized",
		})
		return
	}

	mentions, err := helpers.GetMentions(commentInfo.HTMLText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChatComment Failed to get chat mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat mentions",
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

	dgraphChat, err := business.GetDgraphChatBasicByUUID(ctx, commentInfo.ChatUuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || (dgraphChat.From.DeletedAt != nil && !dgraphChat.From.DeletedAt.IsZero()) || (dgraphChat.To != nil && dgraphChat.To.DeletedAt != nil && !dgraphChat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to get dgraph chat info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph chat info",
			"err": err,
		})
		return
	}

	mentions, err := helpers.GetMentions(commentInfo.HTMLText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to get chat mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chat mentions",
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

	err = business.UpdateChatComment(ctx, commentUUID, &commentInfo, mentionsDgraphUsersList, dgraphChat, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChatComment Failed to update comment in dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update comment in dgraph",
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

	reactionInfo.ReactionDgraphUid, err = business.CreateOrUpdateChatCommentReaction(ctx, &reactionInfo, dgraphComment, &userInfo.UserDgraphInfo)
	if err != nil || reactionInfo.ReactionDgraphUid == "" {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateOrUpdateChatCommentReaction Failed to create/update reaction on comment err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to create/update reaction on comment",
			"err": err,
		})
		return
	}

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

	dgraphReaction, err := reactionBusiness.GetReactionNodeByDgraphUID(ctx, reactionInfo.ReactionDgraphUid)
	if err != nil || dgraphReaction == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentChat Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	if dgraphReaction.AddedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Unauthorized",
		})
		return
	}

	commentDgraphInfo, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, reactionInfo.Uuid, userInfo.UserDgraphInfo.Uid)

	if err != nil || commentDgraphInfo == nil || commentDgraphInfo.Chat.DM.ParticipantIsMember == 0 || (commentDgraphInfo.Chat.From.DeletedAt != nil && !commentDgraphInfo.Chat.From.DeletedAt.IsZero()) || (commentDgraphInfo.Chat.To != nil && commentDgraphInfo.Chat.To.DeletedAt != nil && !commentDgraphInfo.Chat.To.DeletedAt.IsZero()) {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentChat Failed to get comment dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get comment dgraph info",
			"err": err,
		})
		return
	}

	err = business.DeleteReactionOnCommentChat(ctx, commentDgraphInfo, dgraphReaction)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeleteReactionOnCommentChat Failed to delete comment reaction info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete comment reaction",
			"err": err,
		})
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

	if dgraphDm == nil || dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	var uerToken adapter.UserTokenOutput

	uerToken.TokenString, uerToken.AlreadyExisted, err = business.MakeVideoCall(ctx, makeVideoCallInfo.GrpUuid, &userInfo.UserDgraphInfo, false, makeVideoCallInfo.AudioEnabled, makeVideoCallInfo.VideoEnabled, true)

	if err != nil {

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

	uerToken.TokenString, uerToken.AlreadyExisted, err = business.MakeVideoCall(ctx, grpId, &userInfo.UserDgraphInfo, false, makeVideoCallInfo.AudioEnabled, makeVideoCallInfo.VideoEnabled, false)

	if err != nil {

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
	limitValue := queryParams["pageSize"]

	pageIndex := 0
	pageSize := 20

	if len(pageValue) > 0 {
		pageInt, err := strconv.Atoi(pageValue[0])
		if err == nil {
			pageIndex = pageInt
		}
	}

	if len(limitValue) > 0 {
		limitInt, err := strconv.Atoi(limitValue[0])
		if err == nil {
			pageSize = limitInt
		}
	}

	grpIdString := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, chatUUIDString)

	recordingsPagination, err := business.GetDgraphDmRecordingListFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, startDate, endDate, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingList Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dm dgraph info",
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
	limitValue := queryParams["pageSize"]

	pageIndex := 0
	pageSize := 20

	if len(pageValue) > 0 {
		pageInt, err := strconv.Atoi(pageValue[0])
		if err == nil {
			pageIndex = pageInt
		}
	}

	if len(limitValue) > 0 {
		limitInt, err := strconv.Atoi(limitValue[0])
		if err == nil {
			pageSize = limitInt
		}
	}

	recordingsPagination, err := business.GetDgraphDmRecordingListFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIdString, startDate, endDate, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingList Failed to get dm dgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dm dgraph info",
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
	pageSizeStr := queryParams["pageSize"]
	pageIndexStr := queryParams["pageIndex"]
	egressIdString := chi.URLParam(r, "egress_id")

	chatUUIDString := chi.URLParam(r, "chat_uuid")

	var err error

	if len(pageSizeStr) == 0 || len(pageIndexStr) == 0 || len(chatUUIDString) == 0 || len(egressIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	grpIdString := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, chatUUIDString)

	pageSize := 0

	if len(pageSizeStr) != 0 {
		pageSize, err = strconv.Atoi(pageSizeStr[0])
		if err != nil {
			if err != nil {

				helpers.LogErrorWithContext(ctx,
					"controllers/GetChatRecordingTranscript Failed to parse pageSize query param to int err: %+v",
					err)

				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "Failed to parse filters query param",
					"err": err,
				})
				return

			}
		}
	}

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
			"msg": "Failed to get dm dgraph info",
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
	pageSizeStr := queryParams["pageSize"]
	pageIndexStr := queryParams["pageIndex"]
	egressIdString := chi.URLParam(r, "egress_id")

	grpIdString := chi.URLParam(r, "grp_id")

	var err error

	if len(pageSizeStr) == 0 || len(pageIndexStr) == 0 || len(grpIdString) == 0 || len(egressIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	pageSize := 0

	if len(pageSizeStr) != 0 {
		pageSize, err = strconv.Atoi(pageSizeStr[0])
		if err != nil {
			if err != nil {

				helpers.LogErrorWithContext(ctx,
					"controllers/GetGrpChatRecordingTranscript Failed to parse pageSize query param to int err: %+v",
					err)

				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "Failed to parse filters query param",
					"err": err,
				})
				return

			}
		}
	}

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
			"msg": "Failed to get dm dgraph info",
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
