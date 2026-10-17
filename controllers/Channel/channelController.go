package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	business "github.com/akashc777/OneCamp/business/Channel"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChannelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	channelDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const MAX_CH_HANDLE_LENGTH = 25
const CH_HANDLE_REGEX = `[^a-zA-Z0-9- ]`

func CreateChannel(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var channelInfo adapter.InputCreateChannel

	err := json.NewDecoder(r.Body).Decode(&channelInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChannel Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if len(channelInfo.ChannelName) == 0 || !helpers.IsValidName(channelInfo.ChannelName) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid channelInfo",
			"status": "failed",
		})
		return
	}

	err, channelUUID := business.CreateChannel(ctx, &channelInfo, &userInfo)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateChannel Failed to create channel err: %+v",
			err)

		// A duplicate name is the user's business, not the server's, and it is the ONE failure
		// here they can act on. 409, and a message naming the channel, because the client
		// renders this text verbatim into a toast.
		if helpers.IsUniqueViolation(err) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":    "A channel named \"" + channelInfo.ChannelName + "\" already exists.",
				"status": "failed",
			})
			return
		}

		// Anything else is ours. 500, not 400: the request was well-formed and the server could
		// not carry it out, and calling that a client error sends people looking in the wrong
		// place.
		//
		// The error object is deliberately NOT in the body. This branch used to return
		//     "msg": "Failed to parse the body of the req", "err": err
		// which was wrong twice over. The message was copied from the decode branch above, so a
		// user whose channel could not be created was told their request was unparseable — and
		// the client shows that text to them. And a *pq.Error marshals its exported fields, so
		// the response carried the constraint name, the table and column, and Postgres' own
		// source file and line.
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "Could not create the channel. Please try again.",
			"status": "failed",
		})
		return

	}

	var channelCreated dgraphStruct.DgraphChannel

	channelCreated.Uuid = channelUUID.String()
	channelCreated.Name = channelInfo.ChannelName
	channelCreated.IsPrivate = &channelInfo.ChannelPrivate

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "User joined successfully!", "data": channelCreated})
}

func GetIfChannelNameIsAvailable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	chName := r.URL.Query()["ch_name"]

	if len(chName) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	if !helpers.IsValidName(chName[0]) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "channel name should not contain special character",
		})
		return
	}

	exists, err := business.CheckIfChannelExist(ctx, &chName[0])

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetIfChannelNameIsAvailable Failed to check if channel name exist err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to check if channel name exist",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"exists": exists,
	})

}

func GetArchivedChannelListWithLatestPostWithUserIdAndSearchText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputChannelName adapter.InputSearchChannelName

	err := json.NewDecoder(r.Body).Decode(&inputChannelName)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetArchivedChannelListWithLatestPostWithUserIdAndSearchText Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	safeSearch, err := sanitizeSearchOrFail(ctx, w, inputChannelName.SearchText)
	if err != nil {
		return
	}
	userChannels, totalCount, err := business.GetArchivedChannelListWithLatestPostWithUserIdAndSearchText(ctx, userInfo.UserDgraphInfo.Uid, safeSearch, inputChannelName.PageIndex, inputChannelName.PageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetArchivedChannelListWithLatestPostWithUserIdAndSearchText Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if inputChannelName.PageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(inputChannelName.PageSize) - 1) / uint64(inputChannelName.PageSize)
	}

	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got user's channel list successfully",
		"channels_list": userChannels,
		"pageCount":     pageCount,
	})

}

func GetAllActiveChannelList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	pageValue := r.URL.Query()["pageIndex"]
	limitValue := r.URL.Query()["pageSize"]

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

	userChannels, totalCount, err := business.GetAllActiveChannelListWithLatestPost(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllActiveChannelList Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if pageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(pageSize) - 1) / uint64(pageSize)
	}
	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}
	business.MarkGuestAccess(ctx, userChannels...)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got channel list successfully",
		"channels_list": userChannels,
		"pageCount":     pageCount,
	})

}

// RestoreChannel brings an archived channel back, for its admins and the
// workspace's admins: POST /ch/restore {channel_uuid}. Archiving said "It can
// be restored", and nothing in the app could: the update route takes channel
// admins only and no screen offered it. Restoring revives its history in
// search and the AI's memory (business.UpdateChannelInfo), and tells its
// members' apps.
func RestoreChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var body struct {
		ChannelUuid string `json:"channel_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	channelUUID, err := uuid.Parse(body.ChannelUuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a channel."})
		return
	}
	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err == nil && (channelInfo == nil || channelInfo.Uuid == "") {
		err = helpers.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/RestoreChannel")
		return
	}
	if channelInfo.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the channel's admins and workspace admins can restore it."})
		return
	}
	if !helpers.IsSoftDeleted(channelInfo.DeletedAt) {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That channel isn't archived."})
		return
	}

	err = business.UpdateChannelInfo(ctx, &adapter.UpdateChannelInfo{
		ChannelUuid:    channelUUID.String(),
		ChannelName:    channelInfo.Name,
		ChannelPrivate: channelInfo.IsPrivate != nil && *channelInfo.IsPrivate,
	}, channelUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/RestoreChannel Failed to restore the channel err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't restore the channel.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Restored #" + channelInfo.Name + "."})
}

// GetDiscoverChannelList is the channel browser's Discover tab: a page of the
// public channels the person could join and isn't in, by name, with ?search=
// matched on the server. GET /ch/discover?pageIndex=&pageSize=&search=
func GetDiscoverChannelList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	pageIndex, pageSize := 0, 20
	if v, err := strconv.Atoi(r.URL.Query().Get("pageIndex")); err == nil && v >= 0 {
		pageIndex = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && v > 0 && v <= 100 {
		pageSize = v
	}
	search := ""
	if raw := strings.TrimSpace(r.URL.Query().Get("search")); raw != "" {
		safe, err := sanitizeSearchOrFail(ctx, w, raw)
		if err != nil {
			return
		}
		search = safe
	}

	channels, totalCount, err := business.GetDiscoverableChannels(ctx, userInfo.UserDgraphInfo.Uid, search, pageIndex, pageSize)
	if err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load the channels you could join. Try again."})
		return
	}
	if channels == nil {
		channels = []*dgraphStruct.DgraphChannel{}
	}
	business.MarkGuestAccess(ctx, channels...)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"channels_list": channels,
		"pageCount":     (uint64(totalCount) + uint64(pageSize) - 1) / uint64(pageSize),
		"has_more":      int64((pageIndex+1)*pageSize) < totalCount,
	})
}

func GetActiveChannelListWithLatestPostWithUserIdAndSearchText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputChannelName adapter.InputSearchChannelName

	err := json.NewDecoder(r.Body).Decode(&inputChannelName)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetActiveChannelListWithLatestPostWithUserIdAndSearchText Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	safeSearch, err := sanitizeSearchOrFail(ctx, w, inputChannelName.SearchText)
	if err != nil {
		return
	}
	userChannels, totalCount, err := business.GetActiveChannelListWithLatestPostWithUserIdAndSearchText(ctx, userInfo.UserDgraphInfo.Uid, safeSearch, inputChannelName.PageIndex, inputChannelName.PageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetActiveChannelListWithLatestPostWithUserIdAndSearchText Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if inputChannelName.PageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(inputChannelName.PageSize) - 1) / uint64(inputChannelName.PageSize)
	}
	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}
	business.MarkGuestAccess(ctx, userChannels...)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got user's channel list successfully",
		"data":          userChannels,
		"channels_list": userChannels, // Ensure compatibility if frontend expects channels_list
		"pageCount":     pageCount,
	})

}

func GetChannelListWithLatestPostWithUserIdAndSearchText(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var inputChannelName adapter.InputSearchChannelName

	err := json.NewDecoder(r.Body).Decode(&inputChannelName)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelListWithLatestPostWithUserIdAndSearchText Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	if inputChannelName.PageSize == 0 {
		inputChannelName.PageSize = 20
	}

	safeSearch, err := sanitizeSearchOrFail(ctx, w, inputChannelName.SearchText)
	if err != nil {
		return
	}
	userChannels, totalCount, err := business.GetChannelListWithLatestPostWithUserIdAndSearchText(ctx, userInfo.UserDgraphInfo.Uid, safeSearch, inputChannelName.PageIndex, inputChannelName.PageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserChannelListWithLatestPost Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if inputChannelName.PageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(inputChannelName.PageSize) - 1) / uint64(inputChannelName.PageSize)
	}

	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}
	business.MarkGuestAccess(ctx, userChannels...)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got user's channel list successfully",
		"channels_list": userChannels,
		"pageCount":     pageCount,
	})

}

func GetChannelInfoByUUIDWithMemberAdminFlag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	channelUUID, err := uuid.Parse(channelUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUIDWithMemberAdminFlag Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelInfo, err := business.GetDgraphChannelInfoByUUIDWithMemberAdminFlag(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUIDWithMemberAdminFlag Failed to channels info form dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to channels info",
			"err": err,
		})
		return

	}

	if channelInfo.IsMember == 0 && *channelInfo.IsPrivate {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUIDWithMemberAdminFlag Unauthorised user trying to access channelInfo")

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized",
		})
		return
	}
	business.MarkGuestAccess(ctx, channelInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":          "got channel's info successfully",
		"channel_info": channelInfo,
	})

}

func GetChannelBasicInfoByUUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	channelUUID, err := uuid.Parse(channelUUIDString)

	// The read the channel's page stands on: an id that names nothing is a
	// 404 and a failed read a 503, never a 400, which the app can only read as
	// "gone" (QA F-08: an unknown channel drew a live page and "a connection
	// problem").
	const where = "controllers/GetChannelBasicInfoByUUID"
	if err != nil {
		helpers.WriteReadFailure(w, r, helpers.ErrNotFound, "channel", where)
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelInfo, err := business.GetBasicDgraphChannelInfoByUUIDAndUpdateLastSeen(ctx, channelUUID, userInfo.UserPostgresInfo.Id, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", where)
		return
	}

	// A channel whose privacy didn't load is private.
	if channelInfo.IsMember == 0 && (channelInfo.IsPrivate == nil || *channelInfo.IsPrivate) {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelBasicInfoByUUID Unauthorised user trying to access channelInfo")

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized",
		})
		return
	}

	if channelInfo.IsMember == 1 {
		var notificationType string
		notificationType, err = userChannelNotificationBusiness.GetNotificationTypeByUserIdAndChannelId(userInfo.UserDgraphInfo.Uuid, channelUUID.String())

		if err != nil {
			helpers.WriteReadFailure(w, r, err, "channel", where)
			return
		}
		channelInfo.NotificationType = notificationType

		channelInfo.CallActive, err = business.GetChannelCallActiveStatus(ctx, channelUUID.String())

		if err != nil {
			helpers.WriteReadFailure(w, r, err, "channel", where)
			return
		}
	}
	// The header's marker that people outside the workspace can read it.
	business.MarkGuestAccess(ctx, channelInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":          "got channel's info successfully",
		"channel_info": channelInfo,
	})

}

// MarkChannelSeen advances the caller's last-seen marker for a channel so the
// unread badge collapses to zero. Called by the FE when the user leaves a
// channel they were actively viewing (page unmount / channel switch), covering
// the case where messages arrived in-session and would otherwise resurrect the
// badge on the next channel-list refetch. Membership-gated: only a member can
// mark a channel seen (private channels are not addressable by non-members).
func MarkChannelSeen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	channelUUID, err := uuid.Parse(channelUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MarkChannelSeen Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	// Verify the caller can see this channel before advancing their marker.
	// A non-member must not be able to mark a private channel seen.
	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MarkChannelSeen Failed to get channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	if channelInfo.IsMember == 0 && channelInfo.IsPrivate != nil && *channelInfo.IsPrivate {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized",
		})
		return
	}

	if err = business.MarkChannelSeen(ctx, userInfo.UserPostgresInfo.Id, channelUUID); err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MarkChannelSeen Failed to mark channel seen err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to mark channel seen",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Channel marked as seen"})
}

func GetUserArchivedChannelListWithLatestPost(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	pageValue := r.URL.Query()["pageIndex"]
	limitValue := r.URL.Query()["pageSize"]

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

	userChannels, totalCount, err := business.GetUserArchivedChannelListWithLatestPost(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserPostgresInfo.Id, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserArchivedChannelListWithLatestPost Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if pageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(pageSize) - 1) / uint64(pageSize)
	}

	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got user's channel list successfully",
		"channels_list": userChannels,
		"pageCount":     pageCount,
	})

}

func GetUserActiveChannelListWithLatestPost(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	pageValue := r.URL.Query()["pageIndex"]
	limitValue := r.URL.Query()["pageSize"]

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

	userChannels, totalCount, err := business.GetUserActiveChannelListWithLatestPost(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserPostgresInfo.Id, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserActiveChannelListWithLatestPost Failed to get user's channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's channel list",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)
	if pageSize > 0 {
		pageCount = (uint64(totalCount) + uint64(pageSize) - 1) / uint64(pageSize)
	}

	if userChannels == nil {
		userChannels = []*dgraphStruct.DgraphChannel{}
	}
	// Each channel's marker that people outside the workspace can read it.
	business.MarkGuestAccess(ctx, userChannels...)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":           "got user's channel list successfully",
		"channels_list": userChannels,
		"pageCount":     pageCount,
	})

}

func UpdateChannelInfo(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var updateChannelNameInfo adapter.UpdateChannelInfo

	err := json.NewDecoder(r.Body).Decode(&updateChannelNameInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChannelInfo Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(updateChannelNameInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChannelInfo Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChannelInfo Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update channel name",
			"err": err,
		})
		return
	}

	if channelInfo.IsAdmin == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChannelInfo Failed to update channel name since user is not a channel moderator")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update channel name",
			"err": err,
		})
		return
	}

	// The demo's own channels stay for every visitor (business/DemoGuard).
	if updateChannelNameInfo.ChannelArchived && demoGuard.KeepsFromVisitor(userInfo.UserPostgresInfo.EmailID, channelInfo.CreatedAt) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "demo", "msg": helpers.DemoSeededMsg})
		return
	}

	// Opening a private channel to the whole workspace, its history and
	// files with it, is a workspace admin's decision; closing one is any
	// moderator's (business/Channel privacy.go).
	privacy := business.PrivacyChangeOf(channelInfo, updateChannelNameInfo.ChannelPrivate)
	if !business.MayChangePrivacy(privacy, userInfo.UserPostgresInfo.IsAdmin) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only a workspace admin can make a private channel public."})
		return
	}

	err = business.UpdateChannelInfo(ctx, &updateChannelNameInfo, channelUUID)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateChannelInfo Failed to create channel err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	// Everyone in the channel is told who changed its privacy, and what that
	// means, by a line in it that notifies nobody.
	if line := business.PrivacyLine(privacy, userInfo.UserDgraphInfo.DisplayName()); line != "" && !updateChannelNameInfo.ChannelArchived {
		if _, perr := botpost.PostToChannel(ctx, channelUUID, line, ""); perr != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateChannelInfo Failed to post the privacy line err: %+v", perr)
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated channel successfully!"})
}

// SetChannelPostPolicy handles updating a channel's posting policy
// (announcement mode). Channel-moderator gated.
// Seams for tests: SetChannelPostPolicy's read of the channel, and its write.
var (
	readPostPolicyChannel = business.GetBasicDgraphChannelInfoByUUID
	setChannelPostPolicy  = business.SetChannelPostPolicy
)

func SetChannelPostPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var body struct {
		ChannelUuid string `json:"channel_uuid"`
		PostPolicy  string `json:"post_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}

	channelUUID, err := uuid.Parse(body.ChannelUuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid channel id"})
		return
	}

	channelInfo, err := readPostPolicyChannel(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/SetChannelPostPolicy")
		return
	}
	if channelInfo.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only channel moderators can change the posting policy"})
		return
	}

	if err := setChannelPostPolicy(ctx, channelUUID, body.PostPolicy); err != nil {
		if errors.Is(err, business.ErrInvalidPostPolicy) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose who can post: everyone, or only the channel's admins."})
			return
		}
		// A write that failed says which store didn't take it: logged, not shown.
		helpers.LogErrorWithContext(ctx, "controllers/SetChannelPostPolicy err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change who can post.")
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Posting policy updated", "data": map[string]string{"post_policy": body.PostPolicy}})
}

// delete channel moderators
func RemoveChannelModerator(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var removeModeratorInfo adapter.InputChannelMemberInfo

	err := json.NewDecoder(r.Body).Decode(&removeModeratorInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelModerators Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(removeModeratorInfo.ChannelUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelModerators Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channel UUID",
			"err": err,
		})
		return
	}

	channelInfo, err := business.GetDgraphChannelInfoByUUIDAndModeratorInfo(ctx, channelUUID, userInfo.UserPostgresInfo.Id, userInfo.UserDgraphInfo.Uid, removeModeratorInfo.UserUuid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/RemoveChannelModerator")
		return
	}

	if channelInfo.CreatedBy.Uuid == removeModeratorInfo.UserUuid {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Cannot remove channel creator as moderator",
			"err": err,
		})
		return
	}

	if len(channelInfo.Moderators) == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemovePostByModerators Failed to remove channel moderator since given user is not moderator")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove channel moderator since given user is not moderator",
			"err": err,
		})
		return
	}

	if channelInfo.IsAdmin == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemovePostByModerators Failed to remnove channel moderator since user is not channel moderator")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete the moderator since user is not a channel moderator",
			"err": err,
		})
		return
	}

	if !keepsAnAdmin(w, r, channelUUID, removeModeratorInfo.UserUuid, "controllers/RemoveChannelModerator") {
		return
	}

	memberDgraphUID := channelInfo.Moderators[0].Uid

	err = business.DeleteChannelModeratorEdge(ctx, channelInfo.Uid, memberDgraphUID, channelInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelModerators Failed to get delete moderator err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete moderator",
			"err": err,
		})
		return
	}

	business.NotifyChannelUpdated(channelUUID.String(), business.ChannelUpdateActionModerators)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed moderator successfully!"})
}

// keepsAnAdmin answers, for a change taking who's admin role away (they
// leave, are removed, or step down), whether the channel keeps an admin
// (business.KeepsAnAdmin), and when it wouldn't, answers 409 "last_admin",
// as a failed read answers by the shared rule; the handler then returns.
func keepsAnAdmin(w http.ResponseWriter, r *http.Request, channelUUID uuid.UUID, who, where string) bool {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	keeps, err := business.KeepsAnAdmin(r.Context(), channelUUID, userInfo.UserDgraphInfo.Uid, who)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", where)
		return false
	}
	if !keeps {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"msg":  "Make someone else an admin first, so the channel keeps one.",
			"code": "last_admin",
		})
	}
	return keeps
}

func RemoveChannelMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var removeMemberInfo adapter.InputChannelMemberInfo

	err := json.NewDecoder(r.Body).Decode(&removeMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelMembers Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(removeMemberInfo.ChannelUuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a channel."})
		return
	}

	channelInfo, err := business.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelUUID, userInfo.UserDgraphInfo.Uid, removeMemberInfo.UserUuid)
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/RemoveChannelMember")
		return
	}

	// Nobody else removes the channel's creator. They may leave it, as anyone
	// may, once someone else is an admin (below): the creator was held in for
	// good (QA F-46).
	if channelInfo.CreatedBy.Uuid == removeMemberInfo.UserUuid && userInfo.UserDgraphInfo.Uuid != removeMemberInfo.UserUuid {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "The channel's creator can't be removed."})
		return
	}

	if channelInfo.IsAdmin == 0 && userInfo.UserDgraphInfo.Uuid != removeMemberInfo.UserUuid {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelMembers Failed to remove channel member since user is not channel moderator")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove channel member since user is not channel moderator",
			"err": err,
		})
		return
	}

	// check if given member is actually member of the channel
	isMemberChannelMember := false
	memberDgraphUID := ""

	for _, member := range channelInfo.Members {
		if member.Uuid == removeMemberInfo.UserUuid {
			isMemberChannelMember = true
			memberDgraphUID = member.Uid
		}
	}

	if !isMemberChannelMember {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelMembers Failed to remove channel member since given user is not channel member")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove channel member since given user is not channel member",
			"err": err,
		})
		return
	}

	// Leaving takes an admin's role with them, so the channel keeps one.
	if !keepsAnAdmin(w, r, channelUUID, removeMemberInfo.UserUuid, "controllers/RemoveChannelMember") {
		return
	}

	err = business.DeleteChannelMemberEdge(ctx, channelInfo.Uid, memberDgraphUID, removeMemberInfo.UserUuid, channelInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelModerators Failed to remove member err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to delete member",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed member successfully!"})
}

// Add channel moderators
func AddChannelModerators(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var AddModeratorInfo adapter.InputChannelMemberInfo

	err := json.NewDecoder(r.Body).Decode(&AddModeratorInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(AddModeratorInfo.ChannelUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to parse channelUUID string to uuid err: %+v",
			err)
		return
	}

	channelInfo, err := business.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelUUID, userInfo.UserDgraphInfo.Uid, AddModeratorInfo.UserUuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add moderator",
			"err": err,
		})
		return
	}

	if channelInfo.IsAdmin == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to add channel moderator since user is not channel moderator")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add the moderator since user is not a channel moderator",
			"err": err,
		})
		return
	}

	if len(channelInfo.Members) == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to add channel moderator since given user is not channel member")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add channel moderator since given user is not channel member",
			"err": err,
		})
		return
	}

	newModeratorDgraphUID := channelInfo.Members[0].Uid

	err = business.AddChannelModeratorEdge(ctx, AddModeratorInfo.ChannelUuid, newModeratorDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelModerators Failed to get add moderator err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add moderator",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added moderator successfully!"})
}

// GetSuggestedChannel handles GET /ch/suggested: the channel Home offers a
// member who is in none, as "Join #general". A member in no channel used to
// see nothing on Home about channels at all. data is null when there is none
// to offer, and Home then offers the channel list alone.
func GetSuggestedChannel(w http.ResponseWriter, r *http.Request) {
	ch, ok, err := business.SuggestedChannel(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetSuggestedChannel err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't find a channel to suggest."})
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": nil})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": ch})
}

func JoinChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var JoinChannelInfo adapter.InputJoinChannel

	err := json.NewDecoder(r.Body).Decode(&JoinChannelInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/JoinChannel Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(JoinChannelInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/JoinChannel Failed to parse new member stringUUID to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member",
			"err": err,
		})
		return
	}

	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/JoinChannel Failed to get the channel err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member",
			"err": err,
		})
		return
	}

	// Only a live public channel can be joined by oneself; a private one
	// takes someone in it adding you.
	if refusal := business.CanJoin(channelInfo); refusal != nil {
		status := http.StatusForbidden
		if refusal == business.ErrJoinMissing {
			status = http.StatusNotFound
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": refusal.Error()})
		return
	}

	err = business.AddChannelMemberEdge(ctx, channelUUID, &userInfo.UserDgraphInfo, userInfo.UserPostgresInfo.Id)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/JoinChannel Failed to add channel member err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to join channel",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Joined channel successfully!"})

}

func AddChannelMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var AddMemberInfo adapter.InputChannelMemberInfo

	err := json.NewDecoder(r.Body).Decode(&AddMemberInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	channelUUID, err := uuid.Parse(AddMemberInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember Failed to parse new member stringUUID to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member",
			"err": err,
		})
		return
	}

	channelInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	// The answer was read before the error: a channel that isn't there was a
	// panic (F-15). A channel whose privacy didn't load is private.
	if err == nil && channelInfo == nil {
		err = channelDgraphModels.ErrNotFound
	}
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "channel", "controllers/AddChannelMember")
		return
	}
	private := channelInfo.IsPrivate == nil || *channelInfo.IsPrivate
	if (channelInfo.IsMember == 0 && !private) || (channelInfo.IsAdmin == 0 && private) {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember user trying to add is not member or is not mederator if channel is private  err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	newMemberUUID, err := uuid.Parse(AddMemberInfo.UserUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember Failed to parse new member stringUUID to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member",
			"err": err,
		})
		return
	}

	// check if user is valid
	newMemberUserInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, AddMemberInfo.UserUuid)

	if newMemberUserInfo == nil || err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember Failed to get new member userInfo err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add member",
			"err": err,
		})
		return
	}

	if newMemberUserInfo.IsExternal {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Cannot add external users to channels",
		})
		return
	}

	err = business.AddChannelMemberEdge(ctx, channelUUID, newMemberUserInfo, newMemberUUID)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelMember Failed to add channel member err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add channel member",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added member successfully!"})

}

func PublishTypingInChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var channelTypingInfo adapter.InputPublishTypingInChannel

	err := json.NewDecoder(r.Body).Decode(&channelTypingInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/PublishTypingInChannel Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	isChannelMember := false

	for _, channel := range userInfo.UserDgraphInfo.Channels {
		if channel.Uuid == channelTypingInfo.ChannelUuid {
			isChannelMember = true
		}
	}

	if !isChannelMember {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	business.PublishTypingInChannel(&userInfo.UserDgraphInfo, channelTypingInfo.ChannelUuid)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "published typing successfully!"})

}

func MakeVideoChannelCall(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoChannelCallInfo adapter.InputMakeVideoChannelCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoChannelCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoChannelCall Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}
	channelUUID, err := uuid.Parse(makeVideoChannelCallInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoChannelCall Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	channelDraphInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoChannelCall Failed to get channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	// Said in the person's words: a bare "Not Authorised" became "You don't
	// have permission to do that" in the green room (QA F-22).
	if channelDraphInfo.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Only people in the channel can join its huddle. Join the channel first.",
		})
		return
	}

	var uerToken adapter.UserTokenOutput
	uerToken.TokenString, uerToken.AlreadyExisted, err = business.MakeVideoChannelCall(ctx, channelDraphInfo, &userInfo.UserDgraphInfo, false, makeVideoChannelCallInfo.Language)

	if err != nil {
		if errors.Is(err, liveKitBusiness.ErrRemovedFromCall) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "removed_from_call", "msg": liveKitBusiness.RemovedFromCallMsg})
			return
		}

		helpers.LogErrorWithContext(ctx,
			"controllers/MakeVideoChannelCall Failed to make video call err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to make video call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "genereated token successfully!", "data": uerToken})

}

func StartVideoChannelCallRecording(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoChannelCallInfo adapter.InputMakeVideoChannelCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoChannelCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoChannelCallRecording Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}
	channelUUID, err := uuid.Parse(makeVideoChannelCallInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoChannelCallRecording Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	channelDraphInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoChannelCallRecording Failed to get channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	if channelDraphInfo.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.StartRecordingChannelCall(ctx, makeVideoChannelCallInfo.ChannelUuid, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StartVideoChannelCallRecording Failed to record video call err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to make video call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "started recording sucessfully"})

}

func StopVideoChannelCallRecording(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var makeVideoChannelCallInfo adapter.InputMakeVideoChannelCall

	err := json.NewDecoder(r.Body).Decode(&makeVideoChannelCallInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoChannelCallRecording Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}
	channelUUID, err := uuid.Parse(makeVideoChannelCallInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoChannelCallRecording Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	channelDraphInfo, err := business.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoChannelCallRecording Failed to get channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	if channelDraphInfo.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.StopRecordingChannelCall(ctx, makeVideoChannelCallInfo.ChannelUuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/StopVideoChannelCallRecording Failed to record video call err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to make video call",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "started recording sucessfully"})

}

func GetAllChannelRecordingList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelUUIDString := chi.URLParam(r, "channel_uuid")
	if channelUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllChannelRecordingList Received empty channel uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty channel uuid in the req",
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

	recordingsPagination, err := business.GetChannelAllRecordingList(ctx, channelUUIDString, userInfo.UserDgraphInfo.Uid, startDate, endDate, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllChannelRecordingList Failed to get channel info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info",
			"err": err,
		})
		return
	}

	if recordingsPagination.IsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "got channel info successfully!", "channel_info": recordingsPagination})

}

func GetChannelRecordingTranscript(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	queryParams := r.URL.Query()
	pageSizeStr := queryParams["pageSize"]
	pageIndexStr := queryParams["pageIndex"]
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	egressIdString := chi.URLParam(r, "egress_id")

	if len(pageSizeStr) == 0 || len(pageIndexStr) == 0 || len(channelUUIDString) == 0 || len(egressIdString) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid req",
		})
		return
	}

	channelUUID, err := uuid.Parse(channelUUIDString)

	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Invalid channel uuid",
		})
		return

	}

	pageSize := 0

	if len(pageSizeStr) != 0 {
		pageSize, err = strconv.Atoi(pageSizeStr[0])
		if err != nil {
			if err != nil {

				helpers.LogErrorWithContext(ctx,
					"controllers/GetChannelRecordingTranscript Failed to parse pageSize query param to int err: %+v",
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
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse pageIndex query param: " + pageIndexStr[0],
				"err": err.Error(),
			})
			return
		}
	}

	dgraphChannel, err := business.GetChannelRecordingTranscript(ctx, channelUUID, userInfo.UserDgraphInfo.Uid, egressIdString, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjecGetChannelRecordingTranscript failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "failed to get channel info",
			"err": err,
		})
		return

	}

	if dgraphChannel.IsMember == 0 || len(dgraphChannel.Recordings) == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return

	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphChannel.Recordings[0].TranscriptCount + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "got channel info successfully!",
		"data":      dgraphChannel.Recordings[0],
		"pageCount": pageCount,
	})

}

// sanitizeSearchOrFail runs the canonical search-text sanitiser. On
// invalid input it writes the HTTP error response and returns a non-nil
// error so the caller short-circuits. Empty-after-sanitisation is
// treated as a soft 200 with an empty list — that's better UX than
// surfacing a "search field empty" 400 for an emoji-only search.
//
// The returned string is regex-escaped, so callers can interpolate it
// directly into a Dgraph regexp(...) filter via the existing
// fmt.Sprintf-based business helpers.
func sanitizeSearchOrFail(ctx interface{}, w http.ResponseWriter, raw string) (string, error) {
	safe, err := dgraphquery.SanitizeSearchTerm(raw)
	if err == nil {
		return safe, nil
	}
	if errors.Is(err, dgraphquery.ErrEmpty) {
		// Soft-empty: caller handles by returning empty list.
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"msg":           "empty search",
			"channels_list": []interface{}{},
			"pageCount":     uint64(0),
		})
		return "", err
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
		"msg": "search text is too long or invalid",
	})
	return "", err
}

func GetChannelInfoByUUID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelUUIDString := chi.URLParam(r, "channel_uuid")
	channelUUID, err := uuid.Parse(channelUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUID Failed to parse channelUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channelUUID string to uuid",
			"err": err,
		})
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	channelInfo, err := business.GetDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUID Failed to channels info form dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to channels info",
			"err": err,
		})
		return

	}

	if channelInfo.IsMember == 0 && *channelInfo.IsPrivate {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelInfoByUUID Unauthorised user trying to access channelInfo")

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorized",
		})
		return
	}

	if channelInfo.IsMember == 1 {
		var notificationType string
		notificationType, err = userChannelNotificationBusiness.GetNotificationTypeByUserIdAndChannelId(userInfo.UserDgraphInfo.Uuid, channelUUID.String())

		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetChannelInfoByUUID Failed to get users notificationType err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get users notificationType",
				"err": err,
			})
			return
		}
		channelInfo.NotificationType = notificationType
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":          "got channel's info successfully",
		"channel_info": channelInfo,
	})

}
