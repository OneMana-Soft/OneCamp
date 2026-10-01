package controllers

import (
	"net/http"
	"strconv"
	"time"

	business "github.com/akashc777/OneCamp/business/Activity"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func GetLatestMentions(w http.ResponseWriter, r *http.Request) {
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

	mentionsPage, err := business.GetMentionsByUserId(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestMentions Failed to user's mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to user's mentions",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got mentions successfully!", "data": mentionsPage})
}

func GetLatestComments(w http.ResponseWriter, r *http.Request) {
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

	commentsPage, err := business.GetCommentsByUserId(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestComments Failed to user's comments err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's comments",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got comments successfully!", "data": commentsPage})
}

func GetLatestReactions(w http.ResponseWriter, r *http.Request) {
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

	reactionsPage, err := business.GetReactionsByUserId(ctx, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetLatestReactions Failed to user's reactions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to user's reactions",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got reactions successfully!", "data": reactionsPage})
}

func GetUnifiedActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	timeValue := r.URL.Query()["beforeTime"]
	limitValue := r.URL.Query()["limit"]

	limit := 20
	// Default to current time if not provided
	beforeTime := time.Now()

	if len(limitValue) > 0 {
		limitInt, err := strconv.Atoi(limitValue[0])
		if err == nil {
			limit = limitInt
		}
	}

	if len(timeValue) > 0 && timeValue[0] != "" {
		parsedTime, err := time.Parse(time.RFC3339, timeValue[0])
		if err == nil {
			beforeTime = parsedTime
		}
	}

	unifiedPage, err := business.GetUnifiedActivity(ctx, userInfo.UserPostgresInfo.Id, userInfo.UserDgraphInfo.Uid, beforeTime, limit)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUnifiedActivity Failed to get unified activity err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get unified activity",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got unified activity successfully!", "data": unifiedPage})
}
