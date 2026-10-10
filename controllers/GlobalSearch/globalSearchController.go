package controllers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/GlobalSearch"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	business "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

func GetLatestChatAndCommentsGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChatAndCommentsGlobalSearch Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	chatsAndCommentsPage, err := business.GetLatestChatsAndCommentsFromOpenSearch(ctx, userInfo.UserDgraphInfo.Uuid, searchInfo.SearchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChatAndCommentsGlobalSearch Failed to chats and comments from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chats and comments successfully!", "data": chatsAndCommentsPage})
}

func GetLatestChatAndCommentsGlobalSearchBeforeTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChatAndCommentsGlobalSearchBeforeTime Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	lastChatTime := time.Unix(searchInfo.TimeStamp, 0)

	chatsAndCommentsPage, err := business.GetLatestChatsAndCommentsFromOpenSearchBeforeTime(ctx, userInfo.UserDgraphInfo.Uid, searchInfo.SearchText, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestChatAndCommentsGlobalSearchBeforeTime Failed to chats and comments from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got chats and comments successfully!", "data": chatsAndCommentsPage})
}

func GetLatestPostAndCommentGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearch Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	channelsList, err := channelBusiness.GetUsersChannelListWithPublicChannel(ctx, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearch Failed to channels list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to channels list",
			"err": err,
		})
		return
	}

	postsAndCommentsPage, err := business.GetLatestPostsAndCommentsFromOpenSearch(ctx, channelsList, searchInfo.SearchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearch Failed to get channels from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to to get posts and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got posts and comments successfully!", "data": postsAndCommentsPage})
}

func GetLatestPostAndCommentGlobalSearchBeforeTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearchBeforeTime Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	lastChatTime := time.Unix(searchInfo.TimeStamp, 0)

	channelsList, err := channelBusiness.GetUsersChannelListWithPublicChannel(ctx, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearchBeforeTime Failed to get channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel list",
			"err": err,
		})
		return
	}

	postsAndCommentsPage, err := business.GetLatestPostsAndCommentsFromOpenSearchBeforeTime(ctx, channelsList, searchInfo.SearchText, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestPostAndCommentGlobalSearchBeforeTime Failed to get posts and comments from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get posts and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got posts and comments successfully!", "data": postsAndCommentsPage})
}

func GetLatestAttachmentsGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestAttachmentsGlobalSearch Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	attachmentsPage, err := business.GetLatestAttachmentsFromOpenSearch(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Channels, searchInfo.SearchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestAttachmentsGlobalSearch Failed to get attachments from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get chats and comments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got attachments successfully!", "data": attachmentsPage})
}

func GetLatestAttachmentsGlobalSearchBeforeTime(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	err := json.NewDecoder(r.Body).Decode(&searchInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestAttachmentsGlobalSearchBeforeTime Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err,
		})
		return
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
			"err": err,
		})
		return
	}

	lastChatTime := time.Unix(searchInfo.TimeStamp, 0)

	attachmentsPage, err := business.GetLatestAttachmentsFromOpenSearchBeforeTime(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Channels, searchInfo.SearchText, lastChatTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestAttachmentsGlobalSearchBeforeTime Failed to get attachments from opensearch err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachments",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got attachments successfully!", "data": attachmentsPage})
}
func GetUnifiedGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	if r.Method == http.MethodGet {
		searchInfo.SearchText = chi.URLParam(r, "search_text")
	} else {
		err := json.NewDecoder(r.Body).Decode(&searchInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/GetUnifiedGlobalSearch Failed to parse the body of the req err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse body",
				"err": err,
			})
			return
		}
	}

	searchInfo.SearchText = strings.TrimSpace(searchInfo.SearchText)

	if len(searchInfo.SearchText) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Empty search field",
		})
		return
	}

	searchPage, err := business.GetUnifiedGlobalSearch(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserPostgresInfo.EmailID, userInfo.UserDgraphInfo.Channels, userInfo.UserDgraphInfo.Projects, userInfo.UserDgraphInfo.Teams, searchInfo.SearchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUnifiedGlobalSearch Failed to get unified search results err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get search results",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Search results retrieved successfully!", "data": searchPage})
}
