package controllers

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"

	adapter "github.com/akashc777/OneCamp/adapter/User"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	bulkPostAndChatbusiness "github.com/akashc777/OneCamp/business/BulkPostAndChat"
	business "github.com/akashc777/OneCamp/business/User"
	userChannelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
	userChatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	userProjectNotificationBusiness "github.com/akashc777/OneCamp/business/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
)

const MAX_USERNAME_LENGTH = 25
const USERNAME_REGEX = `[^a-zA-Z0-9- ]`

// getFrontendCookieDomain delegates to the canonical authcookie helper so
// all cookie-construction code uses the same domain-resolution logic.
func getFrontendCookieDomain() string {
	return authcookie.FrontendDomain()
}

// getCookieSecureAndSameSite delegates to the canonical authcookie helper.
func getCookieSecureAndSameSite() (secure bool, sameSite http.SameSite) {
	return authcookie.SecureAndSameSite()
}

func GetLoggedInUserProfile(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": userInfo.UserDgraphInfo})
}

func GetActiveUserEmojiStatus(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var statusDraphInfo *dgraphStruct.DgraphUserStatusEmoji

	if len(userInfo.UserDgraphInfo.StatusEmoji) > 0 {
		statusDraphInfo = userInfo.UserDgraphInfo.StatusEmoji[0]
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": statusDraphInfo})

}

func GetDgraphUserTaskListForKanban(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	queryParams := r.URL.Query()

	filtersParamString := queryParams["filters"]
	var filtersParam []adapter.FilterParam

	if len(filtersParamString) != 0 {
		err := json.Unmarshal([]byte(filtersParamString[0]), &filtersParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphUserTaskListForKanban Failed to parse filters query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}

	}

	var err error

	var filterStrings []string
	currentTime := time.Now()

	for _, param := range filtersParam {

		// Defence: refuse unrecognised column names so a hostile body
		// can't get its `id` interpolated as a DQL keyword.
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}

		var filterValue string
		switch v := param.Value.(type) {
		case string:
			if param.Id == "task_name" {
				safe, sErr := dgraphquery.SanitizeSearchTerm(v)
				if sErr == nil {
					filterValue = fmt.Sprintf(`regexp(%s,  /.*%s.*/i)`, param.Id, safe)
				}
			}
			if param.Id == "overdue" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, currentTime.Format(time.RFC3339Nano))
			}
			if param.Id == "upcoming" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND gt(task_start_date, "%s")`, currentTime.Format(time.RFC3339Nano))
			}

		case []interface{}:
			strValues := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && dgraphquery.IsAllowedFilterValue(s) {
					strValues = append(strValues, s)
				}
			}
			if param.Id == "task_priority" {
				filterValue = fmt.Sprintf(`anyofterms(%s,"%s")`, param.Id, strings.Join(strValues, " "))
			}
			// Built-in and custom statuses; see business/TaskStatus.
			if param.Id == "task_status" {
				filterValue = taskStatusBusiness.FilterClause(strValues)
			}
			if param.Id == "task_project_name" {
				filterValue = fmt.Sprintf(`uid_in(task_project, [%s])`, strings.Join(strValues, ", "))

			}

		}

		if len(filterValue) > 0 {
			filterStrings = append(filterStrings, filterValue)
		}
	}

	filterQuery := strings.Join(filterStrings, " AND ")

	sortParamString := queryParams["sorting"]

	var sortParam []adapter.SortingParam

	if len(sortParamString) != 0 {
		err := json.Unmarshal([]byte(sortParamString[0]), &sortParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphUserTaskListForKanban Failed to parse sorting query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse sorting query param",
				"err": err,
			})
			return

		}

	}

	sortingQuery := ""
	for _, param := range sortParam {
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}
		switch param.Desc {
		case true:
			sortingQuery = fmt.Sprintf("orderdesc: %s", param.Id)
		case false:

			sortingQuery = fmt.Sprintf("orderasc: %s", param.Id)

		}

	}

	if len(sortingQuery) == 0 {
		sortingQuery = "orderdesc: task_created_at"
	}

	dgraphUser, err := business.GetDgraphUserTaskListForKanban(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Uid, filterQuery)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphUserTaskListForKanban Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph user task",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Sucessful got task list",
		"data": dgraphUser,
	})
}

func GetDgraphUserProjectList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	userDgraphInfo, err := business.GetDgraphUserProjectList(ctx, userInfo.UserDgraphInfo.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphUserProjectList Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph user project",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Sucessful got task list",
		"data": userDgraphInfo,
	})

}

func GetDgraphUserTaskList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	queryParams := r.URL.Query()

	filtersParamString := queryParams["filters"]
	var filtersParam []adapter.FilterParam

	if len(filtersParamString) != 0 {
		err := json.Unmarshal([]byte(filtersParamString[0]), &filtersParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphUserTaskList Failed to parse filters query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse filters query param",
				"err": err,
			})
			return

		}

	}

	taskSearchStr := queryParams["taskSearchString"]

	getAll, pageSize, pageIndex, err := helpers.ListPaging(queryParams)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	var filterStrings []string
	currentTime := time.Now()

	if len(taskSearchStr) != 0 {
		safe, sErr := dgraphquery.SanitizeSearchTerm(taskSearchStr[0])
		if sErr != nil {
			if !errors.Is(sErr, dgraphquery.ErrEmpty) {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "search string is too long or invalid",
				})
				return
			}
			// Empty after sanitisation: fall through; search filter
			// won't be appended and the unfiltered task list is
			// returned, which is what the user expects when typing
			// only whitespace.
		} else {
			filterStrings = append(filterStrings, fmt.Sprintf(`regexp(task_name,  /.*%s.*/i)`, safe))
		}
	}

	startDateStrParam := queryParams["startDate"]
	endDateStrParam := queryParams["endDate"]

	if len(startDateStrParam) > 0 && len(endDateStrParam) > 0 {
		startDateStr := startDateStrParam[0]
		endDateStr := endDateStrParam[0]
		if startDateStr != "" && endDateStr != "" {
			// RFC3339 dates only: parse-then-reformat so the value we
			// interpolate cannot contain `"` or DQL operators.
			if s, e := time.Parse(time.RFC3339, startDateStr); e == nil {
				if t, e2 := time.Parse(time.RFC3339, endDateStr); e2 == nil {
					sf := s.Format(time.RFC3339Nano)
					tf := t.Format(time.RFC3339Nano)
					dateFilter := fmt.Sprintf(`(ge(task_due_date, "%s") AND le(task_due_date, "%s")) OR (ge(task_start_date, "%s") AND le(task_start_date, "%s")) OR (le(task_start_date, "%s") AND ge(task_due_date, "%s"))`, sf, tf, sf, tf, tf, sf)
					filterStrings = append(filterStrings, fmt.Sprintf(`(%s)`, dateFilter))
				}
			}
		}
	}

	for _, param := range filtersParam {

		// Defence: refuse unrecognised column names so a hostile body
		// can't get its `id` interpolated as a DQL keyword.
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}

		var filterValue string
		switch v := param.Value.(type) {
		case string:
			if param.Id == "task_name" {
				safe, sErr := dgraphquery.SanitizeSearchTerm(v)
				if sErr == nil {
					filterValue = fmt.Sprintf(`regexp(%s,  /.*%s.*/i)`, param.Id, safe)
				}
			}
			if param.Id == "overdue" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, currentTime.Format(time.RFC3339Nano))
			}
			if param.Id == "upcoming" {
				filterValue = fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND gt(task_start_date, "%s")`, currentTime.Format(time.RFC3339Nano))
			}

		case []interface{}:
			strValues := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && dgraphquery.IsAllowedFilterValue(s) {
					strValues = append(strValues, s)
				}
			}
			if param.Id == "task_priority" {
				filterValue = fmt.Sprintf(`anyofterms(%s,"%s")`, param.Id, strings.Join(strValues, " "))
			}
			// Built-in and custom statuses; see business/TaskStatus.
			if param.Id == "task_status" {
				filterValue = taskStatusBusiness.FilterClause(strValues)
			}
			if param.Id == "task_project_name" {
				filterValue = fmt.Sprintf(`uid_in(task_project, [%s])`, strings.Join(strValues, ", "))

			}

		}

		if len(filterValue) > 0 {
			filterStrings = append(filterStrings, filterValue)
		}
	}

	filterQuery := strings.Join(filterStrings, " AND ")

	sortParamString := queryParams["sorting"]

	var sortParam []adapter.SortingParam

	if len(sortParamString) != 0 {
		err := json.Unmarshal([]byte(sortParamString[0]), &sortParam)
		if err != nil {

			helpers.LogErrorWithContext(ctx,
				"controllers/GetDgraphUserTaskList Failed to parse sorting query param err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to parse sorting query param",
				"err": err,
			})
			return

		}

	}

	sortingQuery := ""
	for _, param := range sortParam {
		if !dgraphquery.IsAllowedColumnName(param.Id) {
			continue
		}
		switch param.Desc {
		case true:
			sortingQuery = fmt.Sprintf("orderdesc: %s", param.Id)
		case false:

			sortingQuery = fmt.Sprintf("orderasc: %s", param.Id)

		}

	}

	if len(sortingQuery) == 0 {
		sortingQuery = "orderdesc: task_created_at"
	}

	dgraphUser, err := business.GetDgraphUserTaskList(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Uid, filterQuery, sortingQuery, pageSize, pageIndex, getAll)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphUserTaskList Failed to get dgraph user task err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph user task",
			"err": err,
		})
		return

	}

	pageCount := uint64(1)

	if pageSize > 0 {
		pageCount = (dgraphUser.TaskCount + uint64(pageSize) - 1) / uint64(pageSize)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":       "Sucessful got task list",
		"data":      dgraphUser,
		"pageCount": pageCount,
	})
}

func GetDgraphUserInfoByUUIDForSidebarNav(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	dgraphUser, err := business.GetDgraphUserInfoByUUIDForSidebarNav(ctx, userInfo.UserPostgresInfo.Id)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDgraphUserInfoByUUIDForSidebarNav Failed to get dgraph user info err: %+v",
			err)
		return
	}

	dgraphUser.IsAdmin = userInfo.UserDgraphInfo.IsAdmin

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": dgraphUser})
}

func AddChannelToUserFav(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelId := chi.URLParam(r, "channel_id")

	channelUUID, err := uuid.Parse(channelId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get parse channelUUID",
			"err": err,
		})
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	dgrpahChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelToUserFav Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info err",
			"err": err,
		})
		return
	}

	if dgrpahChannel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "User is not member of channel",
		})
		return
	}

	err = business.AddFavChannel(ctx, userInfo.UserDgraphInfo.Uuid, dgrpahChannel.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AddChannelToUserFav Failed to add fav channel err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to add fav channel",
			"err": err,
		})
		return
	}

	// Invalidate this user's cached basic-channel-info so the next fetch
	// recomputes ch_is_user_fav. Without this, the header star (which reads
	// the cached ChannelBasicInfo) shows stale "not favourite" until the TTL
	// expires, even though the favourite is persisted and shown in the sidebar.
	_ = redisStore.Delete(ctx, registry.ChannelBasicInfo, []string{channelUUID.String(), userInfo.UserDgraphInfo.Uid})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})

}

func RemoveChannelToUserFav(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channelId := chi.URLParam(r, "channel_id")

	channelUUID, err := uuid.Parse(channelId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get parse channelUUID",
			"err": err,
		})
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	dgrpahChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelToUserFav Failed to get channel info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get channel info err",
			"err": err,
		})
		return
	}

	if dgrpahChannel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "User is not member of channel",
		})
		return
	}

	err = business.RemoveFavChannel(ctx, userInfo.UserDgraphInfo.Uid, dgrpahChannel.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveChannelToUserFav Failed to remove fav channel err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove fav channel",
			"err": err,
		})
		return
	}

	// Invalidate this user's cached basic-channel-info so the next fetch
	// recomputes ch_is_user_fav (mirrors the add path).
	_ = redisStore.Delete(ctx, registry.ChannelBasicInfo, []string{channelUUID.String(), userInfo.UserDgraphInfo.Uid})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})

}

func GetUsersListWhoDontBelongToTheDM(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	grpIDString := chi.URLParam(r, "grp_id")

	dgraphDm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheDM Failed to get Dm info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get Dm info",
			"err": err,
		})
		return
	}

	if dgraphDm == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Group chat not found",
		})
		return
	}

	if dgraphDm.ParticipantIsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	dgraphUsers, err := business.GetUsersListWhoDontBelongToTheDM(ctx, dgraphDm.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheDM Failed to users list  err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to users list",
			"err": err,
		})
		return
	}

	// This list feeds the add-to-group-chat picker. Among bot principals it
	// includes ONLY the shared AI coworker (so a member can add the AI to a
	// group, then @mention it there), gated on the coworker feature being on so
	// there is no dangling affordance. Per-AGENT bots are channel teammates and
	// are NOT group-addable here, so they are dropped. Humans are unaffected.
	dgraphUsers = filterGroupAddBots(ctx, dgraphUsers)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":   "Got users list!",
		"users": dgraphUsers,
	})

}

// filterGroupAddBots keeps only the shared coworker bot among bot principals
// (and only when the coworker feature is enabled); all human users pass through.
func filterGroupAddBots(ctx context.Context, users []*dgraphStruct.DgraphUser) []*dgraphStruct.DgraphUser {
	coworkerOn := false
	if s, serr := aiModels.GetSettings(ctx); serr == nil && s != nil && s.Enabled && s.CoworkerEnabled {
		coworkerOn = true
	}
	sharedBotUUID := ""
	if bot := business.GetAutomationBot(ctx); bot != nil {
		sharedBotUUID = bot.UUID
	}
	filtered := users[:0]
	for _, u := range users {
		if u == nil {
			continue
		}
		if u.IsBot {
			if coworkerOn && u.Uuid == sharedBotUUID {
				filtered = append(filtered, u)
			}
			continue
		}
		filtered = append(filtered, u)
	}
	return filtered
}

func GetUsersListWhoDontBelongToTheTeam(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	teamUUIDString := chi.URLParam(r, "team_uuid")

	_, err := uuid.Parse(teamUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheTeam Failed to parse teamUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse teamUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphTeam, err := teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, teamUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheTeam Failed to get dgraph team info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return
	}

	if dgraphTeam.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return

	}

	dgraphUsers, err := business.GetUsersListWhoDontBelongToTheTeam(ctx, teamUUIDString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheTeam Failed to get users list err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":   "Got users list!",
		"users": dgraphUsers,
	})

}

func GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	projectUUIDString := chi.URLParam(r, "project_uuid")

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam Failed to parse projectUUID string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse projectUUID string to uuid",
			"err": err,
		})
		return
	}

	dgraphProject, err := projectBusiness.GetDgraphProjectInfoAndTeamAdminFlag(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam Failed to get dgraph project info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dgraph team info",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectAdmin == 0 && dgraphProject.Team.IsAdmin == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return

	}

	dgraphUsers, err := business.GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam(ctx, dgraphProject.Team.Uuid, dgraphProject.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam Failed to get users list err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":   "Got users list!",
		"users": dgraphUsers,
	})

}

func GetProfileByUserId(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userId := chi.URLParam(r, "user_id")

	_, err := uuid.Parse(userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProfileByUserId Failed to parse userId string to uuid err: %+v",
			err)
		return
	}

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	otherUserInfo, err := business.GetDgraphUserInfoByUUID(ctx, userId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProfileByUserId Failed to get user info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user info err",
			"err": err,
		})
		return
	}

	grpId := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, otherUserInfo.Uuid)

	notificationType, err := userChatNotificationBusiness.GetNotificationTypeByUserIdAndToUserId(ctx, userInfo.UserDgraphInfo.Uuid, grpId)
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
		otherUserInfo.NotificationType = notificationType
	}

	otherUserInfo.CallActive, err = business.GetDmCallStatus(ctx, grpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProfileByUserId Failed to get call status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get call status",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": otherUserInfo})
}

func GetIfUserNameIsAvailable(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	var rawUserName adapter.InputUserNameValid

	err := json.NewDecoder(r.Body).Decode(&rawUserName)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetIfUserNameIsValid Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	exists, err := business.CheckIfUserExistByUserName(ctx, &rawUserName.Uname)

	if !exists || err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"unameExist": false,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"unameExist": true,
	})

}

//func Login(w http.ResponseWriter, r *http.Request) {
//	ctx := r.Context()
//	var userInfo models.User
//
//	err := json.NewDecoder(r.Body).Decode(&userInfo)
//	if err != nil {
//
//		helpers.LogErrorWithContext(ctx,
//			"controllers/user Failed to parse the body of the req err: %+v",
//			err)
//
//		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
//			"msg": "Failed to parse the body of the req",
//			"err": err,
//		})
//		return
//
//	}
//
//	dbUserInfo, tokenString, err := business.LoginUserByEmailID(userInfo.EmailID)
//
//	if err != nil {
//
//		helpers.LogErrorWithContext(ctx,
//			"controllers/user Failed to login err: %+v",
//			err)
//
//		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
//			"msg": "Failed to parse the body of the req",
//			"err": err,
//		})
//		return
//
//	}
//	currentTime := time.Now()
//	dgraphUser := dgraphStruct.DgraphUser{
//		Uuid:      dbUserInfo.Id.String(),
//		UserName:  dbUserInfo.UserName,
//		EmailID:   dbUserInfo.EmailID,
//		UpdatedAt: &currentTime,
//		CreatedAt: &dbUserInfo.CreatedAt,
//	}
//	_, err = business.CreateOrUpdateDgraphUser(ctx, &dgraphUser)
//
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"controllers/UpdateUNameByEmailID Failed to create user in dgraph err: %+v",
//			err)
//
//		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
//			"msg": "Failed to create user in dgraph",
//			"err": err,
//		})
//		return
//	}
//
//	http.SetCookie(w, &http.Cookie{
//		Name:    "Authorization",
//		Value:   tokenString,
//		Expires: time.Now().Add(time.Hour),
//	})
//
//	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
//		"msg":         "successfully loggedIn !",
//		"tokenString": tokenString,
//	})
//}

func UploadFile(w http.ResponseWriter, r *http.Request) {

	uploadLimitMB := settingsBusiness.UploadLimitMB()
	MultipartUploadLimit := 1024 * 1024 * uploadLimitMB
	// Parse the form data.
	ctx := r.Context()
	// Cap the request body BEFORE multipart parsing so a malicious
	// client can't push gigabytes through ParseMultipartForm and
	// either exhaust /tmp (multipart spills past MaxMemory) or pin
	// memory. MaxBytesReader rejects with a clean 413 once the cap is
	// breached.
	r.Body = http.MaxBytesReader(w, r.Body, MultipartUploadLimit)

	// Cap the in-memory portion of the parse to a small, fixed value.
	// Anything bigger spills to /tmp; combined with MaxBytesReader
	// above, we never accept more than MultipartUploadLimit total.
	const multipartInMemoryCap = 8 * 1024 * 1024 // 8 MiB
	err := r.ParseMultipartForm(multipartInMemoryCap)
	if err != nil {
		// http.MaxBytesReader surfaces an *http.MaxBytesError when the body
		// exceeds the cap. Distinguish that (413 + the actual limit so the
		// client can show a precise message) from a genuinely malformed form
		// (400). This is what powers the "File exceeds the 10 MB limit"
		// toast on the FE.
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || strings.Contains(err.Error(), "request body too large") {
			helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
				"msg":             fmt.Sprintf("File exceeds the %d MB upload limit", uploadLimitMB),
				"error":           "file_too_large",
				"upload_limit_mb": uploadLimitMB,
			})
			return
		}
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadMedia Failed to ParseMultipartForm err: %+v",
			err)

		er := errors.New("Failed to ParseMultipartForm file")
		helpers.ErrorJSON(w, er, http.StatusBadRequest)
		return
	}

	// Get the file handle.
	file, fHeader, err := r.FormFile("file")
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadMedia Failed to get csv file err: %+v",
			err)

		er := errors.New("Failed to get media file")
		helpers.ErrorJSON(w, er, http.StatusBadRequest)
		return
	}

	jsonData := r.FormValue("jsonData")
	var uploadData adapter.AddOrRemoveAttachmentInput
	err = json.Unmarshal([]byte(jsonData), &uploadData)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadMedia Failed to parse json data err: %+v",
			err)

		er := errors.New("Failed to upload the file")
		helpers.ErrorJSON(w, er, http.StatusBadRequest)
		return
	}

	defer func() {
		err = file.Close()
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/UploadMedia Failed to temp file err: %+v",
				err)

			er := errors.New("Failed to temp file")
			helpers.ErrorJSON(w, er, http.StatusBadRequest)
			return
		}
	}()

	var objUUID uuid.UUID

	if len(uploadData.ChannelUuids) == 0 && len(uploadData.ChatUuids) == 0 {
		if uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_CHANNEL &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_CHAT &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_PROJECT &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_GRP_CHAT &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_DOC &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_BOARD &&
			uploadData.SrcKey != postgressStruct.ATTACHMENT_SRC_PUBLIC {

			er := errors.New("Invalid src key")
			helpers.ErrorJSON(w, er, http.StatusBadRequest)

			return
		}

		objUUID, err = business.UploadUserFile(ctx, file, fHeader.Size, fHeader.Filename, uploadData.SrcKey, uploadData.SrcValue)
	}

	if len(uploadData.ChannelUuids) > 0 || len(uploadData.ChatUuids) > 0 {

		objUUID, err = business.UploadUserFileToChannelsAndChats(ctx, file, fHeader.Size, fHeader.Filename, uploadData.ChannelUuids, uploadData.ChatUuids)

	}

	if err != nil || objUUID == uuid.Nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UploadMedia Failed to upload the file err: %+v",
			err)

		er := errors.New("Failed to upload the file")
		helpers.ErrorJSON(w, er, http.StatusBadRequest)
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":         "successfully Uploaded !",
		"object_uuid": objUUID,
	})
}

func OAuthLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := chi.URLParam(r, "oauth_provider")

	// Sanitize the redirect URI: only accept FE-host-anchored targets so a
	// crafted ?redirect_uri=https://attacker.com can't turn this endpoint
	// into an open redirector after a successful login.
	requested := strings.TrimSpace(r.URL.Query().Get("redirect_uri"))
	redirectURL := authService.FrontendBaseURL() + "/app"
	if requested != "" && authService.IsRedirectAllowed(requested) {
		redirectURL = requested
	}

	// Mint a single-use state token, stash the redirect target in Redis
	// keyed by the token. This is the standard OAuth/OIDC anti-CSRF
	// pattern; the IdP echoes the token back on /oauth_callback/{provider}
	// where we exchange it for the redirect URL we stored.
	state, err := authService.GenerateAndStoreSSOState(ctx, redirectURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/OAuthLogin state mint err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to start sign-in",
		})
		return
	}

	url, err := business.OAuthLogin(ctx, state, provider)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to OAuth login URL",
			"err": err,
		})
		return
	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func LogOutUser(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	feDomain := getFrontendCookieDomain()
	secure, sameSite := getCookieSecureAndSameSite()

	// Diagnostic logging for cookies
	cookies := r.Cookies()
	var cookieNames []string
	for _, c := range cookies {
		cookieNames = append(cookieNames, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}

	// Always clear cookies in the browser - moved to top for robustness
	domainsToClear := []string{"", feDomain}
	if strings.Count(feDomain, ".") >= 2 {
		parts := strings.Split(feDomain, ".")
		parentDomain := "." + strings.Join(parts[len(parts)-2:], ".")
		domainsToClear = append(domainsToClear, parentDomain)
	}

	cookieNamesToClear := []string{"Authorization", "AuthToken", "RefreshToken", "DeviceId"}

	for _, domainName := range domainsToClear {
		for _, cookieName := range cookieNamesToClear {
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    "",
				Expires:  time.Now().Add(-time.Hour * 24),
				Path:     "/",
				HttpOnly: cookieName == "DeviceId",
				Secure:   secure,
				SameSite: sameSite,
				Domain:   domainName,
			})
		}
	}

	// Also clear whatever cookies were sent (legacy loop)
	for _, cookie := range cookies {
		c := &http.Cookie{
			Name:     cookie.Name,
			Value:    "",
			Path:     "/",
			Domain:   feDomain,
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
			SameSite: sameSite,
			Secure:   secure,
		}
		http.SetCookie(w, c)
	}

	dsc, err := r.Cookie("DeviceId")
	if err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"msg": "Logged out (DeviceID not found, cookies cleared anyway)",
		})
		return
	}

	deviceId := dsc.Value

	valUserInfo := ctx.Value(helpers.UserInfoContextKey)
	if valUserInfo == nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "User already logged out, cookies cleared"})
		return
	}
	userInfo, ok := valUserInfo.(models.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Invalid user info, session cleared"})
		return
	}

	err = redisStore.Delete(ctx, registry.UserRefreshToken, []string{userInfo.UserPostgresInfo.Id.String(), deviceId})

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/LogOutUser Failed to remove refresh key in redis err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to remove refresh key in redis",
			"err": err,
		})
		return
	}

	err = userFCMtokenBusiness.DeleteByUserIdAndDeviceId(ctx, userInfo.UserPostgresInfo.Id.String(), deviceId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/LogOutUser Failed to remove fcm token err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to remove fcm token",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "successfully cleared server cookies !"})

}

func RefreshToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	feDomain := getFrontendCookieDomain()
	secure, sameSite := getCookieSecureAndSameSite()

	//tsc, err := r.Cookie("RefreshToken")
	//if err != nil {
	//
	//	helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
	//		"msg": "Not Authorised",
	//	})
	//	return
	//}

	dsc, err := r.Cookie("DeviceId")

	if err != nil {

		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	deviceId := dsc.Value

	//refreshTokenString := tsc.Value

	refreshTokenInRedis, _, err := redisStore.GetString(ctx, registry.UserRefreshToken, []string{userInfo.UserPostgresInfo.Id.String(), deviceId})

	if err != nil || len(refreshTokenInRedis) == 0 {

		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	//if refreshTokenString != refreshTokenInRedis {
	//	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{})
	//	return
	//}

	refreshTokenTTLDuration := registry.UserRefreshToken.TTL

	authExpiryTime := time.Now().Add(time.Minute * 6)
	authCookieExpiryTime := time.Now().Add(time.Minute * 5)
	refreshExpiryTime := time.Now().Add(refreshTokenTTLDuration)

	authTokenString, newRefreshTokenString, err := business.ResetToken(ctx, userInfo.UserPostgresInfo.Id.String(), deviceId, authExpiryTime.Unix(), refreshExpiryTime.Unix())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RefreshToken Failed generate new token err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed generate new token err",
			"err": err,
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "Authorization",
		Value:    authTokenString,
		Expires:  authCookieExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "RefreshToken",
		Value:    newRefreshTokenString,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "DeviceId",
		Value:    deviceId,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{})

}

func OAuthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := chi.URLParam(r, "oauth_provider")
	state := r.FormValue("state")
	oauthCode := r.FormValue("code")

	// Defense-in-depth: a missing code means the IdP never authenticated
	// the user (consent denied, navigation accident). Send them back to
	// the login page rather than to a half-broken callback.
	if state == "" || oauthCode == "" {
		http.Redirect(w, r, authService.FrontendBaseURL()+"/?error=unauthorized&message=missing-callback-params", http.StatusFound)
		return
	}

	// Atomically consume the state — single-use, prevents CSRF and replay.
	// The redirectURL was stashed at /oauth_login time, so we never have to
	// trust whatever the IdP echoed back.
	redirectURL, err := authService.ConsumeSSOState(ctx, state)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/OAuthCallback consume state err: %+v", err)
		http.Redirect(w, r, authService.FrontendBaseURL()+"/?error=unauthorized&message=invalid-state", http.StatusFound)
		return
	}

	// Re-validate the redirect target — defensive against a corrupted Redis
	// entry. Falls back to /app on the FE base.
	if !authService.IsRedirectAllowed(redirectURL) {
		redirectURL = authService.FrontendBaseURL() + "/app"
	}

	emailID, uname, err := business.OAuthCallback(ctx, oauthCode, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/OAuthCallback Failed to get user email err: %+v",
			err)

		// Redirect to the login page (root) — not /app — with error info
		// so the frontend can show feedback without a double-redirect
		loginURL := getLoginRedirectURL(redirectURL)
		errorRedirect := appendQueryParam(loginURL, "error", "unauthorized")
		errorRedirect = appendQueryParam(errorRedirect, "message", "Your account is not authorized to access this workspace. Please contact your administrator for an invitation.")
		http.Redirect(w, r, errorRedirect, http.StatusFound)
		return
	}

	refreshTokenTTLDuration := registry.UserRefreshToken.TTL

	authExpiryTime := time.Now().Add(time.Minute * 6)
	authCookieExpiryTime := time.Now().Add(time.Minute * 5)
	refreshExpiryTime := time.Now().Add(refreshTokenTTLDuration)

	_, authTokenString, refreshTokenString, deviceId, err := business.LoginUserByEmailID(ctx, emailID, uname, authExpiryTime.Unix(), refreshExpiryTime.Unix())

	if len(authTokenString) == 0 {
		errorRedirect := appendQueryParam(redirectURL, "error", "login_failed")
		errorRedirect = appendQueryParam(errorRedirect, "message", "Login failed. Please try again.")
		http.Redirect(w, r, errorRedirect, http.StatusFound)
		return
	}
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/OAuthCallback Failed to login err: %+v",
			err)

		errorRedirect := appendQueryParam(redirectURL, "error", "login_failed")
		errorRedirect = appendQueryParam(errorRedirect, "message", "Something went wrong during login. Please try again.")
		http.Redirect(w, r, errorRedirect, http.StatusFound)
		return

	}
	feDomain := getFrontendCookieDomain()
	secure, sameSite := getCookieSecureAndSameSite()

	http.SetCookie(w, &http.Cookie{
		Name:     "Authorization",
		Value:    authTokenString,
		Expires:  authCookieExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "RefreshToken",
		Value:    refreshTokenString,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "DeviceId",
		Value:    deviceId,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})

	// Best-effort audit: record which provider this login came through.
	// "google" / "github" — the URL-param value is canonical.
	if user, err := userDomain.GetUserByEmailId(ctx, &emailID); err == nil && user != nil {
		_ = userDomain.RecordLoginMethod(ctx, user.Id, provider)
	}

	http.Redirect(w, r, redirectURL, http.StatusFound)

}

// appendQueryParam safely adds a query parameter to a URL string,
// handling both URLs that already have query params and those that don't.
func appendQueryParam(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		// Fallback: just append manually
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		return rawURL + sep + url.QueryEscape(key) + "=" + url.QueryEscape(value)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// getLoginRedirectURL extracts the base URL (scheme + host) from a redirect URI
// to safely redirect users to the login page on failure without losing query params
// by redirecting into a protected route.
func getLoginRedirectURL(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Host == "" {
		// Fallback to origin or root if parsing fails
		return "/"
	}
	u.Path = "/"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func UsersListNotBelongToChannelId(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	channelId := chi.URLParam(r, "channel_id")
	if channelId == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/UsersListNotBelongToChannelId Received empty channelID key in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty channelID key in the req",
		})
		return

	}

	channelUUID, err := uuid.Parse(channelId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UsersListNotBelongToChannelId Failed to parse channel string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse channel string to uuid",
			"err": err,
		})
		return
	}

	usersList, err := business.UsersListNotExistInGivenChannel(ctx, channelUUID.String())
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UsersListNotBelongToChannelId Failed to get users list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":   "Got the users list!",
		"users": usersList,
	})
}

func GetUserRecordingsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	queryParams := r.URL.Query()
	startDate := queryParams.Get("startDate")
	endDate := queryParams.Get("endDate")

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

	recordingsPagination, err := business.GetUserRecordingsList(ctx, userInfo.UserDgraphInfo.Uid, startDate, endDate, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserRecordingsList Failed to get users recording list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users recording list",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": recordingsPagination})

}

func AllUsersList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	usersList, err := business.GetDgraphAllUsersList(ctx)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/AllUsersList Failed to get users list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return

	}

	// This list feeds the global @mention typeahead. Among bot principals it
	// includes ONLY the shared AI coworker bot (gated on the coworker feature
	// being on, so there is never a dangling "@AI" that can't respond). Per-AGENT
	// bots are deliberately excluded here: an agent is a channel teammate, so it
	// is surfaced only in the @mention typeahead of channels it has been added to
	// (see the channel mention-agents endpoint), not globally. Human users are
	// unaffected.
	coworkerOn := false
	if s, serr := aiModels.GetSettings(ctx); serr == nil && s != nil && s.Enabled && s.CoworkerEnabled {
		coworkerOn = true
	}
	sharedBotUUID := ""
	if bot := business.GetAutomationBot(ctx); bot != nil {
		sharedBotUUID = bot.UUID
	}
	filtered := usersList[:0]
	for _, u := range usersList {
		if u == nil {
			continue
		}
		if u.IsBot {
			// Keep only the shared coworker bot, and only when coworker is on.
			if coworkerOn && u.Uuid == sharedBotUUID {
				filtered = append(filtered, u)
			}
			continue
		}
		filtered = append(filtered, u)
	}
	usersList = filtered

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":   "Got the all users list!",
		"users": usersList,
	})
}

func GetPublicFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetPublicFile Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_PUBLIC)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetPublicFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_PUBLIC {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetPublicFile Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func GetGroupChatFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	grpIDString := chi.URLParam(r, "grp_id")
	if grpIDString == "" {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty chat uuid in the req",
		})
		return

	}

	dgraphDm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, grpIDString)

	if err != nil || dgraphDm == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetGroupChatFile Failed to get chat dm info err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get dm info",
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

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_GRP_CHAT)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetGroupChatFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_GRP_CHAT || attachmentPostgresInfo.SrcValue != grpIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGroupChatFile Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func GetDocFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocFile Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	doclUUIDString := chi.URLParam(r, "doc_uuid")
	if doclUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocFile Received empty doc uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty doc uuid in the req",
		})
		return

	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_DOC)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_DOC || attachmentPostgresInfo.SrcValue != doclUUIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	dgraphDoc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, doclUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocFile Failed to get docInfo err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get docInfo",
			"err": err,
		})
		return
	}

	if *dgraphDoc.IsPrivate == true && dgraphDoc.HasEditAccess == 0 && dgraphDoc.HasReadAccess == 0 && dgraphDoc.HasCommentAccess == 0 && dgraphDoc.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocFile Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func GetDocAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocAttachment Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	doclUUIDString := chi.URLParam(r, "doc_uuid")
	if doclUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocAttachment Received empty doc uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty doc uuid in the req",
		})
		return

	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_DOC)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocAttachment Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_DOC || attachmentPostgresInfo.SrcValue != doclUUIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	dgraphDoc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, doclUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocAttachment Failed to get docInfo err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get docInfo",
			"err": err,
		})
		return
	}

	if *dgraphDoc.IsPrivate == true && dgraphDoc.HasEditAccess == 0 && dgraphDoc.HasReadAccess == 0 && dgraphDoc.HasCommentAccess == 0 && dgraphDoc.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetDocAttachment Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// GetBoardAttachment serves a board image from MinIO (presigned redirect),
// enforcing board access. Mirrors GetDocAttachment.
func GetBoardAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Received empty object uuid in the req"})
		return
	}
	boardUUIDString := chi.URLParam(r, "board_uuid")
	if boardUUIDString == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Received empty board uuid in the req"})
		return
	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_BOARD)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardAttachment Failed to get attachmentInfo err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get attachmentInfo", "err": err})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_BOARD || attachmentPostgresInfo.SrcValue != boardUUIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	dgraphBoard, err := boardBusiness.GetBasicBoardByUUID(ctx, boardUUIDString, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardAttachment Failed to get boardInfo err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get boardInfo", "err": err})
		return
	}

	isPrivate := dgraphBoard.IsPrivate != nil && *dgraphBoard.IsPrivate
	isOwner := dgraphBoard.CreatedBy != nil && dgraphBoard.CreatedBy.Uuid == userInfo.UserDgraphInfo.Uuid
	if isPrivate && dgraphBoard.HasEditAccess == 0 && dgraphBoard.HasReadAccess == 0 && dgraphBoard.HasCommentAccess == 0 && !isOwner {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Not Authorised"})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBoardAttachment Failed to get url err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to get url", "err": err})
		return
	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func GetChatFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatFile Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	chatUUIDString := chi.URLParam(r, "chat_uuid")
	if chatUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatFile Received empty chat uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty chat uuid in the req",
		})
		return

	}

	_, err := uuid.Parse(chatUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatFile Failed to parse chat string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse chat string to uuid",
			"err": err,
		})
		return
	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_CHAT)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	grpString := helpers.GetGroupingId(chatUUIDString, userInfo.UserDgraphInfo.Uuid)

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_CHAT || attachmentPostgresInfo.SrcValue != grpString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatFile Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func GetAllUserEmojiStatusList(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	userDgraphInfo, err := business.GetAllUserEmojiStatusList(ctx, userInfo.UserDgraphInfo.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllUserEmojiStatusList Failed to get user's status list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get user's status list",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": userDgraphInfo.StatusEmoji})

}

func ClearUserEmojiStatus(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	if len(userInfo.UserDgraphInfo.StatusEmoji) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "No status to clear",
		})
		return
	}

	err := business.ClearUserEmojiStatus(ctx, userInfo.UserDgraphInfo.StatusEmoji[0].Uid, userInfo.UserDgraphInfo.Uuid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllUserEmojiStatusList Failed to clear user's status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to clear user's status",
			"err": err,
		})
		return

	}
}

func UpdateUserEmojiStatus(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userInputStatus adapter.UserEmojiStatusInput

	err := json.NewDecoder(r.Body).Decode(&userInputStatus)
	if err != nil {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return

	}

	err = business.UpdateUserEmojiStatus(ctx, &userInfo.UserDgraphInfo, &userInputStatus)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserEmojiStatus Failed to update user status err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to update user status",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "status updated",
		"status": "success",
	})

}

func GetGrpChatRecordingURL(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	grpIdString := chi.URLParam(r, "grp_id")
	if grpIdString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingURL Received empty group chat uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty group chat uuid in the req",
		})
		return

	}

	egressIdString := chi.URLParam(r, "egress_id")
	if egressIdString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingURL Received empty egressId in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty egress id in the req",
		})
		return

	}

	dgraphRecording, err := recordingBusiness.GetDgraphDmRecordingInfoByEgressId(ctx, egressIdString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingURL Failed to recording info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to recording info",
			"err": err,
		})
		return

	}

	if dgraphRecording.Dm.ParticipantIsMember == 0 || dgraphRecording.Dm.GroupingId != grpIdString {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	url, err := business.GetRecordingURLByObjectName(ctx, dgraphRecording.ObjectKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetGrpChatRecordingURL Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})
}

func GetChatRecordingURL(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	chatUUIDString := chi.URLParam(r, "chat_uuid")
	if chatUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingURL Received empty chat uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty chat uuid in the req",
		})
		return

	}

	egressIdString := chi.URLParam(r, "egress_id")
	if egressIdString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingURL Received empty egressId in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty egress id in the req",
		})
		return

	}

	grpId := helpers.GetGroupingId(chatUUIDString, userInfo.UserDgraphInfo.Uuid)

	dgraphRecording, err := recordingBusiness.GetDgraphDmRecordingInfoByEgressId(ctx, egressIdString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingURL Failed to recording info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to recording info",
			"err": err,
		})
		return

	}

	if dgraphRecording.Dm.ParticipantIsMember == 0 || dgraphRecording.Dm.GroupingId != grpId {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	url, err := business.GetRecordingURLByObjectName(ctx, dgraphRecording.ObjectKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChatRecordingURL Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})
}

func GetChannelRecordingURL(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	channelUUIDString := chi.URLParam(r, "channel_uuid")
	if channelUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelRecordingURL Received empty channel uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty channel uuid in the req",
		})
		return

	}

	egressIdString := chi.URLParam(r, "egress_id")
	if egressIdString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelRecordingURL Received empty egressId in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty egress id in the req",
		})
		return

	}

	dgraphRecording, err := recordingBusiness.GetDgraphChannelRecordingInfoByEgressId(ctx, egressIdString, userInfo.UserDgraphInfo.Uid)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelRecordingURL Failed to recording info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to recording info",
			"err": err,
		})
		return

	}

	if dgraphRecording.Channel.IsMember == 0 || dgraphRecording.Channel.Uuid != channelUUIDString {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not authorised",
		})
		return
	}

	url, err := business.GetRecordingURLByObjectName(ctx, dgraphRecording.ObjectKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelRecordingURL Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})
}

func GetChannelFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	channelUUIDString := chi.URLParam(r, "channel_uuid")
	if channelUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Received empty project uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty project uuid in the req",
		})
		return

	}

	channelUUID, err := uuid.Parse(channelUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Failed to parse project string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse project string to uuid",
			"err": err,
		})
		return
	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_CHANNEL)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_CHANNEL || attachmentPostgresInfo.SrcValue != channelUUIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	dgraphChannel, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Failed to get projectInfo err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get projectInfo",
			"err": err,
		})
		return
	}

	if dgraphChannel.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": " Not authorised",
			"err": err,
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetChannelFile Failed to get url err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func FwdUserMessage(w http.ResponseWriter, r *http.Request) {

	ctx := r.Context()

	var userFwdMsgRawInfo adapter.UserFwdMsgInput
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	err := json.NewDecoder(r.Body).Decode(&userFwdMsgRawInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/FwdUserMessage Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	if len(userFwdMsgRawInfo.FwdTo) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Fwd list is empty",
		})
		return
	}

	var userDgraphPost *dgraphStruct.DgraphPost

	var userDgraphChat *dgraphStruct.DgraphChat

	if userFwdMsgRawInfo.ChannelUuid != "" {
		channelDgraph, err := channelBusiness.GetBasicChannelAndPostInfoByUUID(ctx, userFwdMsgRawInfo.ChannelUuid, userFwdMsgRawInfo.PostUuid, userInfo.UserDgraphInfo.Uid)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/FwdUserMessage failed to forward message err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to forward message",
				"err": err,
			})
			return
		}

		if channelDgraph.IsMember == 0 || len(channelDgraph.Posts) == 0 {

			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"msg": "Not authorised",
			})
			return
		}

		userDgraphPost = channelDgraph.Posts[0]
	}

	if userFwdMsgRawInfo.ChatUuid != "" {

		userDgraphChat, err = chatBusiness.GetDgraphChatBasicByUUID(ctx, userFwdMsgRawInfo.ChatUuid, userInfo.UserDgraphInfo.Uid)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/FwdUserMessage Failed to get chat info err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to get chat info req",
				"err": err,
			})
			return
		}

		// Verify the user is a member of the DM that contains this chat
		if userDgraphChat.DM == nil || userDgraphChat.DM.ParticipantIsMember == 0 {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"msg": "Not authorised to forward this message",
			})
			return
		}
	}

	// Bug 3: Verify user is a member of each group chat destination
	for _, fwdTarget := range userFwdMsgRawInfo.FwdTo {
		if fwdTarget.GrpId == "" {
			continue
		}
		grpDmInfo, errGrp := chatBusiness.GetDgraphDmBasicByGrpId(ctx, fwdTarget.GrpId, userInfo.UserDgraphInfo.Uid)
		if errGrp != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/FwdUserMessage Failed to verify group chat membership err: %+v",
				errGrp)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to verify group chat membership",
			})
			return
		}
		if grpDmInfo == nil || grpDmInfo.ParticipantIsMember == 0 {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"msg": "Not authorised to forward to this group",
			})
			return
		}
	}

	// get mentions
	mentions, err := helpers.GetMentions(userFwdMsgRawInfo.HtmlText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/FwdUserMessage Failed to get post mentions err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get post mentions",
			"err": err,
		})
		return
	}
	// validate mentions (will be sent via FE)

	mentionsDgraphUsersList, err := business.GetDgraphUserInfoByUUIDs(ctx, mentions)

	if len(mentionsDgraphUsersList) != len(mentions) {

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Got unregistered users in mentions",
			"err": err,
		})
		return
	}

	var channelDgraphUIDs []string

	for _, fwdChannel := range userFwdMsgRawInfo.FwdTo {
		if fwdChannel.ChannelDgraphUid != "" {
			channelDgraphUIDs = append(channelDgraphUIDs, fwdChannel.ChannelDgraphUid)
		}
	}

	if len(channelDgraphUIDs) > 0 {

		dgraphChannelList, err := channelBusiness.GetChannelListWithMemberFlag(ctx, userInfo.UserDgraphInfo.Uid, channelDgraphUIDs)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"controllers/FwdUserMessage failed to get channel into err: %+v",
				err)

			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": "Failed to forward message",
				"err": err,
			})
			return
		}

		for _, dgraphChannel := range dgraphChannelList {

			if dgraphChannel.IsMember == 0 {

				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
					"msg": "User is not member of channel",
					"err": err,
				})
				return

			}

		}
	}

	err = bulkPostAndChatbusiness.BulkPostAndChatForward(ctx, &userInfo.UserDgraphInfo, &userFwdMsgRawInfo, mentionsDgraphUsersList, userDgraphPost, userDgraphChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/FwdUserMessage failed to forward message err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to forward message",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Forwarded messaage",
		"status": "success",
	})

}

func GetProjectFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	objectUUIDString := chi.URLParam(r, "obj_uuid")
	if objectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectFile Received empty object uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty object uuid in the req",
		})
		return

	}

	projectUUIDString := chi.URLParam(r, "project_uuid")
	if projectUUIDString == "" {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectFile Received empty project uuid in the req")

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Received empty project uuid in the req",
		})
		return

	}

	_, err := uuid.Parse(projectUUIDString)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectFile Failed to parse project string to uuid err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse project string to uuid",
			"err": err,
		})
		return
	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objectUUIDString, postgressStruct.ATTACHMENT_SRC_PROJECT)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectFile Failed to get attachmentInfo from postgres err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get attachmentInfo",
			"err": err,
		})
		return
	}

	if attachmentPostgresInfo.SrcKey != postgressStruct.ATTACHMENT_SRC_PROJECT || attachmentPostgresInfo.SrcValue != projectUUIDString {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
			"err": err,
		})
		return
	}

	dgraphProject, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, projectUUIDString, userInfo.UserDgraphInfo.Uid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetProjectFile Failed to get projectInfo err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": " Failed to get projectInfo",
			"err": err,
		})
		return
	}

	if dgraphProject.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": " Not authorised",
			"err": err,
		})
		return
	}

	url, err := business.GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetFile Failed to login err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get url",
			"err": err,
		})
		return

	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Got the url!",
		"url": url,
	})

}

func GetAllUsersList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	dgraphUsers, hasMore, err := business.GetAllUsersListFromDgraph(ctx, pageIndex, pageSize)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllUsersList Failed to get users list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get users list",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":      "Got users list",
		"data":     dgraphUsers,
		"has_more": hasMore,
	})
}

func UpdateUserStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userStatusInfo adapter.InputUserStatus
	err := json.NewDecoder(r.Body).Decode(&userStatusInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProfile Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	var userStatus string

	switch userStatusInfo.Status {

	case dgraphStruct.USER_OPT_STATUS_OFFLINE:
		userStatus = dgraphStruct.USER_OPT_STATUS_OFFLINE
	case dgraphStruct.USER_OPT_STATUS_ONLINE:
		userStatus = dgraphStruct.USER_OPT_STATUS_ONLINE
	}

	if len(userStatus) == 0 {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	err = business.UpdateUserStatusInDgraph(ctx, userStatus, userInfo.UserDgraphInfo.Uuid)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProfile Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user's status",
		"status": "success",
	})

}

func UpdateUserProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	var userRawInfo adapter.InputEditUserProfile

	err := json.NewDecoder(r.Body).Decode(&userRawInfo)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProfile Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"err":    err,
			"status": "failed",
		})
		return
	}

	usernameSpecialCharPattern := regexp.MustCompile(USERNAME_REGEX)
	if usernameSpecialCharPattern.MatchString(userRawInfo.UserName) || len(userRawInfo.UserName) >= MAX_USERNAME_LENGTH {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    fmt.Sprintf("username contains special character or has length greater than %v", MAX_USERNAME_LENGTH),
			"err":    err,
			"status": "failed",
		})
		return
	}

	currentTime := time.Now()
	// err = business.UpdateUNameByEmailID(userInfo.UserPostgresInfo.EmailID, userRawInfo.UserName, currentTime)

	// if err != nil {

	// 	helpers.LogErrorWithContext(ctx,
	// 		"controllers/UpdateUserProfile Failed to update user name err: %+v", //'/
	// 		err)

	// 	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
	// 		"msg":    "Failed to update user name",
	// 		"err":    err,
	// 		"status": "failed",
	// 	})
	// 	return

	// }
	userInfo.UserDgraphInfo.UserName = userRawInfo.UserName
	userInfo.UserDgraphInfo.Hobbies = userRawInfo.Hobbies
	userInfo.UserDgraphInfo.Title = userRawInfo.Title
	userInfo.UserDgraphInfo.UserFullName = userRawInfo.UserFullName
	userInfo.UserDgraphInfo.Uuid = userInfo.UserPostgresInfo.Id.String()
	userInfo.UserDgraphInfo.ProfileKey = &userRawInfo.ProfilePicKey
	userInfo.UserDgraphInfo.CreatedAt = &userInfo.UserPostgresInfo.CreatedAt
	userInfo.UserDgraphInfo.UpdatedAt = &currentTime
	userInfo.UserDgraphInfo.EmailID = userInfo.UserPostgresInfo.EmailID
	userInfo.UserDgraphInfo.AppLang = userRawInfo.AppLang
	userInfo.UserDgraphInfo.Status = userRawInfo.Status

	// if len(userInfo.UserPostgresInfo.UserName) != 0 {
	// 	userDgraphInfo, errTemp := domain.GetDgraphUserInfoByUUID(ctx, userInfo.UserPostgresInfo.Id.String())

	// 	err = errTemp
	// 	if err != nil {
	// 		helpers.LogErrorWithContext(ctx,
	// 			"controllers/UpdateUserProfile Failed to user info in dgraph err: %+v",
	// 			err)

	// 		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
	// 			"msg":    "Failed to user info in dgraph err",
	// 			"err":    err,
	// 			"status": "failed",
	// 		})
	// 		return
	// 	}
	// 	userInfo.UserDgraphInfo.Status = userDgraphInfo.Status
	// }

	if len(userInfo.UserDgraphInfo.Status) == 0 {
		userInfo.UserDgraphInfo.Status = dgraphStruct.USER_OPT_STATUS_ONLINE
	}

	_, err = business.CreateOrUpdateDgraphUser(ctx, &userInfo.UserDgraphInfo)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProfile Failed to create user in dgraph err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to create user in dgraph",
			"err":    err,
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user",
		"status": "success",
	})
}

func UpdateUserTheme(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	var themeInput adapter.InputUserTheme

	err := json.NewDecoder(r.Body).Decode(&themeInput)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserTheme Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"err":    err,
			"status": "failed",
		})
		return
	}

	err = business.UpdateUserTheme(ctx, userInfo.UserDgraphInfo.Uuid, themeInput.ThemeColor, themeInput.ThemeMode)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserTheme Failed to update user theme err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to update user theme",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user theme",
		"status": "success",
	})
}

func GetUserPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

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

	dgraphUser, err := business.GetUsersPosts(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.Uid, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetUserPosts Failed to get users post err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to get users post",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data":   dgraphUser,
		"status": "success",
	})

}

func UpdateUserFCMToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	deviceId := ctx.Value(helpers.DeviceIdContextKey).(string)

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userFCMToken adapter.InputUserFCMToken

	err := json.NewDecoder(r.Body).Decode(&userFCMToken)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserFCMToken Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	err = userFCMtokenBusiness.CreateOrUpdateUserFCMToken(ctx, userInfo.UserDgraphInfo.Uuid, deviceId, userFCMToken.FCMToken)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserFCMToken Failed to create/update user's fcm token err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to create/update user's fcm token",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user's token",
		"status": "success",
	})
}

func UpdateUserGroupChatNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userNotificationStatus adapter.InputUserGroupChatNotification

	err := json.NewDecoder(r.Body).Decode(&userNotificationStatus)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserGroupChatNotification Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	dgraphDm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, userNotificationStatus.GrpId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserGroupChatNotification failed to get group dm info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "failed to get group dm info",
			"err": err,
		})

		return
	}

	if err != nil || dgraphDm.ParticipantIsMember == 0 {

		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	if userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_ALL && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_MENTION && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_BLOCK {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Invalid notification status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid notification status",
			"status": "failed",
		})
		return
	}

	err = userChatNotificationBusiness.UpdateChatNotificationType(ctx, userInfo.UserDgraphInfo.Uuid, userNotificationStatus.GrpId, userNotificationStatus.NotificationType)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Failed to update user's chat notification err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to update user's chat notification",
			"status": "failed",
		})
		return
	}
}

func UpdateUserChatNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userNotificationStatus adapter.InputUserChatNotification

	err := json.NewDecoder(r.Body).Decode(&userNotificationStatus)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	toUserId, err := uuid.Parse(userNotificationStatus.ToUserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification failed to parse toUserId err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "failed to parse toUserId",
			"err": err,
		})

		return
	}

	toUserInfo, err := business.GetUserByUUID(ctx, toUserId)
	if err != nil || toUserInfo == nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Invalid userID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid userID",
			"status": "failed",
		})
		return
	}

	if userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_ALL && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_MENTION && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_BLOCK {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Invalid notification status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid notification status",
			"status": "failed",
		})
		return
	}

	grpId := helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, userNotificationStatus.ToUserUuid)
	err = userChatNotificationBusiness.UpdateChatNotificationType(ctx, userInfo.UserDgraphInfo.Uuid, grpId, userNotificationStatus.NotificationType)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChatNotification Failed to update user's chat notification err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to update user's chat notification",
			"status": "failed",
		})
		return
	}
}

func DeactivateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var inputUserInfoRaw adapter.InputUserUUID

	err := json.NewDecoder(r.Body).Decode(&inputUserInfoRaw)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeactivateUser Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	inputUserUUID, err := uuid.Parse(inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DeactivateUser Failed to parse userUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID in req",
			"err": err,
		})
		return
	}

	if userInfo.UserPostgresInfo.Id == inputUserUUID {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to block self",
		})
		return
	}

	err = business.DeactivateUser(ctx, inputUserUUID)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/DeactivateUser Failed to block user err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to block user",
		})
		return
	}

	go userFCMtokenBusiness.DeleteByUserId(inputUserInfoRaw.UserUuid)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Blocked user",
		"status": "success",
	})

}

func CreateAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var inputUserInfoRaw adapter.InputUserUUID

	err := json.NewDecoder(r.Body).Decode(&inputUserInfoRaw)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/CreateAdmin Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	_, err = uuid.Parse(inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateAdmin Failed to parse userUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID in req",
			"err": err,
		})
		return
	}

	userDgraphInfo, err := business.GetDgraphUserInfoByUUID(ctx, inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateAdmin Failed to get userDgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get userDgraph info",
			"err": err,
		})
		return
	}

	err = business.CreateAdminUser(ctx, userDgraphInfo.EmailID, inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/CreateAdmin Failed to create admin user err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get create admin user",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Created admin user",
		"status": "success",
	})
}

func FwdUserAndChannelList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var searchTextInputRaw adapter.SearchInputFwdMsgText

	err := json.NewDecoder(r.Body).Decode(&searchTextInputRaw)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/FwdUserAndChannelList Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	// Sanitise before passing into Dgraph regex filters in domain layer.
	safeSearch, sErr := dgraphquery.SanitizeSearchTerm(searchTextInputRaw.SearchText)
	if sErr != nil {
		if errors.Is(sErr, dgraphquery.ErrEmpty) {
			helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "empty search", "data": nil})
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "search text is too long or invalid"})
		return
	}

	resultList, err := business.GetChannelsAndUsers(ctx, userInfo.UserDgraphInfo.Uid, userInfo.UserDgraphInfo.Uuid, safeSearch)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/FwdUserAndChannelList Failed to get user and channel list err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get search result",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data":   resultList,
		"status": "success",
	})

}

func RemoveAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var inputUserInfoRaw adapter.InputUserUUID

	err := json.NewDecoder(r.Body).Decode(&inputUserInfoRaw)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdmin Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	inputUserUUID, err := uuid.Parse(inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdmin Failed to parse userUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID in req",
			"err": err,
		})
		return
	}

	if userInfo.UserPostgresInfo.Id == inputUserUUID {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Can't remove self from admin",
		})
		return
	}

	userDgraphInfo, err := business.GetDgraphUserInfoByUUID(ctx, inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdmin Failed to get userDgraph info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get userDgraph info",
			"err": err,
		})
		return
	}

	err = business.HardDeleteAdminUserByEmailId(ctx, userDgraphInfo.EmailID, inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/RemoveAdmin Failed to delete admin user err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to get delete admin user",
			"err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Deleted admin user",
		"status": "success",
	})
}

func GetAllAdminUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	usersInfo, hasMore, err := business.GetAllAdminUsers(ctx, pageIndex, pageSize)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/GetAllAdminUsers Failed to get all admin users err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to get all admin users",
			"status": "failed",
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":      "Got admin users",
		"data":     usersInfo,
		"has_more": hasMore,
	})

}

func GetSelfAdminProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Self user profile",
		"data": userInfo.UserPostgresInfo,
	})
}

func ActivateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var inputUserInfoRaw adapter.InputUserUUID

	err := json.NewDecoder(r.Body).Decode(&inputUserInfoRaw)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ActivateUser Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	inputUserUUID, err := uuid.Parse(inputUserInfoRaw.UserUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ActivateUser Failed to parse userUUID err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID in req",
			"err": err,
		})
		return
	}

	err = business.ActivateUser(ctx, inputUserUUID)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/ActivateUser Failed to unblock user err: %+v",
			err)
		if helpers.WriteSeatLimit(w, err) {
			return
		}

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to unblock user",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Unblocked user",
		"status": "success",
	})

}

func UpdateUserProjectNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userNotificationStatus adapter.InputUserProjectNotification

	err := json.NewDecoder(r.Body).Decode(&userNotificationStatus)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProjectNotification Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	_, err = uuid.Parse(userNotificationStatus.ProjectUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProjectNotification failed to parse projectId err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "failed to parse projectId",
			"err": err,
		})

		return
	}

	projectDgraphInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, userNotificationStatus.ProjectUuid, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProjectNotification failed to get project info err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "failed to get project info",
			"status": "failed",
		})
		return
	}

	if projectDgraphInfo.IsProjectMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	if userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_ALL && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_MENTION && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_BLOCK {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProjectNotification Invalid notification status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid notification status",
			"status": "failed",
		})
		return
	}

	err = userProjectNotificationBusiness.UpdateProjectNotificationType(userInfo.UserDgraphInfo.Uuid, userNotificationStatus.ProjectUuid, userNotificationStatus.NotificationType)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserProjectNotification Failed to update user's project notification err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to update user's project notification",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user's project notifiaction",
		"status": "success",
	})
}

func UpdateUserChannelNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var userNotificationStatus adapter.InputUserChannelNotification

	err := json.NewDecoder(r.Body).Decode(&userNotificationStatus)
	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChannelNotification Failed to parse the body of the req err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse the body of the req",
			"status": "failed",
		})
		return
	}

	channelUUID, err := uuid.Parse(userNotificationStatus.ChannelUuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChannelNotification failed to parse channelId err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "failed to parse channelId",
			"err": err,
		})

		return
	}

	channelDgraphInfo, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChannelNotification Invalid channelId err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid channelID",
			"status": "failed",
		})
		return
	}

	if channelDgraphInfo.IsMember == 0 {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg": "Not Authorised",
		})
		return
	}

	if userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_ALL && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_MENTION && userNotificationStatus.NotificationType != postgressStruct.NOTIFICATION_TYPE_BLOCK {
		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChannelNotification Invalid notification status err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Invalid notification status",
			"status": "failed",
		})
		return
	}

	err = userChannelNotificationBusiness.UpdateChannelNotificationType(userInfo.UserDgraphInfo.Uuid, userNotificationStatus.ChannelUuid, userNotificationStatus.NotificationType)

	if err != nil {

		helpers.LogErrorWithContext(ctx,
			"controllers/UpdateUserChannelNotification Failed to update user's channel notification err: %+v",
			err)

		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to update user's channel notification",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Updated user's channel notifiaction",
		"status": "success",
	})
}

func GetAllInvitations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	invitations, err := business.GetAllInvitations(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    err.Error(),
			"status": "failed",
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   invitations,
	})
}

func AddInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var requestBody struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.Email = strings.TrimSpace(strings.ToLower(requestBody.Email))

	if requestBody.Email == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "email is required",
			"status": "failed",
		})
		return
	}

	// A full workspace says so now, not after the person has accepted and
	// chosen a password.
	if helpers.WriteSeatLimit(w, userDomain.EnsureSeatAvailable(ctx)) {
		return
	}

	// Generate secure invitation token
	token, err := generateInvitationToken()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AddInvitation Failed to generate token err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to generate invitation token",
			"status": "failed",
		})
		return
	}

	expiresAt := time.Now().Add(7 * 24 * time.Hour) // 7 days

	err = business.AddInvitationWithToken(ctx, requestBody.Email, userInfo.UserPostgresInfo.Id, token, expiresAt)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    err.Error(),
			"status": "failed",
		})
		return
	}

	// Send invitation email asynchronously
	go sendInvitationEmail(requestBody.Email, token)

	respondInvitation(w, false, invitationLink(authService.FrontendBaseURL(), token))
}

func ResendInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var requestBody struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.Email = strings.TrimSpace(strings.ToLower(requestBody.Email))

	// Generate new token
	newToken, err := generateInvitationToken()
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to generate new token",
			"status": "failed",
		})
		return
	}

	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	// Update the invitation with new token, reset status to sent
	err = business.UpdateInvitationTokenByEmail(ctx, requestBody.Email, newToken, expiresAt)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    err.Error(),
			"status": "failed",
		})
		return
	}

	// Send email asynchronously
	go sendInvitationEmail(requestBody.Email, newToken)

	respondInvitation(w, true, invitationLink(authService.FrontendBaseURL(), newToken))
}

func DeleteInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := chi.URLParam(r, "email")
	if email == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "email is required",
			"status": "failed",
		})
		return
	}

	err := business.DeleteInvitationByEmail(ctx, email)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    err.Error(),
			"status": "failed",
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "invitation deleted successfully",
	})
}

// --- Demo Login ---

// demoLogins bounds demo login attempts per IP.
//
// Ten a minute is generous for a person and cheap to refuse for anything else.
// The limiter is the shared one: this was forty lines of sync.Map, a mutex per
// entry and a ticker goroutine that woke every ten minutes for the life of the
// process to sweep a map that is empty on every install that is not the demo.
var demoLogins = helpers.NewRateLimiter(10, time.Minute)

// isDemoRateLimited returns true if the given IP has exceeded the demo login rate limit.
func isDemoRateLimited(ip string) bool {
	return !demoLogins.Allow(ip)
}

// getClientIP extracts the real client IP, respecting X-Forwarded-For from Traefik.
func getClientIP(r *http.Request) string {
	// Traefik sets X-Forwarded-For
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}
	if xff := r.Header.Get("X-Real-Ip"); xff != "" {
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// DemoLogin handles one-click demo access without requiring OAuth.
// It is gated behind the DEMO_MODE=true env var and rate-limited per IP.
// The demo user is auto-created on first call if not present.
func DemoLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Guard: only enabled when DEMO_MODE=true
	if !helpers.DemoMode() {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "Not found",
		})
		return
	}

	// 2. Rate limit by client IP
	clientIP := getClientIP(r)
	if isDemoRateLimited(clientIP) {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{
			"msg": "Too many demo login attempts. Please try again in a minute.",
		})
		return
	}

	// 3. Resolve demo user credentials from env
	demoEmail := strings.TrimSpace(os.Getenv("DEMO_USER_EMAIL"))
	demoName := strings.TrimSpace(os.Getenv("DEMO_USER_NAME"))
	if demoEmail == "" || demoName == "" {
		helpers.LogErrorWithContext(ctx,
			"controllers/DemoLogin DEMO_USER_EMAIL or DEMO_USER_NAME not configured")
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Demo is not configured properly",
		})
		return
	}

	// 4. Auto-create the demo user if they don't exist yet
	//    (mirrors the OAuthCallback new-user flow)
	emailID, uname := demoEmail, demoName
	err := business.EnsureDemoUserExists(ctx, emailID, uname)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/DemoLogin Failed to ensure demo user exists err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to set up demo account",
		})
		return
	}

	// 5. Generate tokens (same flow as OAuthCallback)
	refreshTokenTTLDuration := registry.UserRefreshToken.TTL
	authExpiryTime := time.Now().Add(time.Minute * 6)
	authCookieExpiryTime := time.Now().Add(time.Minute * 5)
	refreshExpiryTime := time.Now().Add(refreshTokenTTLDuration)

	_, authTokenString, refreshTokenString, deviceId, err := business.LoginUserByEmailID(
		ctx, emailID, uname, authExpiryTime.Unix(), refreshExpiryTime.Unix(),
	)

	if err != nil || len(authTokenString) == 0 {
		helpers.LogErrorWithContext(ctx,
			"controllers/DemoLogin Failed to generate tokens for demo user err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Failed to create demo session",
		})
		return
	}

	// 6. Set auth cookies (identical to OAuthCallback)
	feDomain := getFrontendCookieDomain()
	secure, sameSite := getCookieSecureAndSameSite()

	http.SetCookie(w, &http.Cookie{
		Name:     "Authorization",
		Value:    authTokenString,
		Expires:  authCookieExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "RefreshToken",
		Value:    refreshTokenString,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "DeviceId",
		Value:    deviceId,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Demo login successful",
		"status": "success",
	})
}

// --- Email Invitation Helpers ---

func generateInvitationToken() (string, error) {
	b := make([]byte, 32)
	_, err := cryptoRand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// invitationLink is the URL an invitation resolves to, built on the same base
// the password-reset and SSO redirects use.
//
// WHY THIS EXISTS. The email used to build its link from FE_HOST_DOMAIN as
// written, and as written it has no scheme: make install sets it to
// onecamp.<domain>. An <a href="onecamp.example.com/signup?token=..."> is a
// RELATIVE link to a mail client, so every invitation this server ever sent
// pointed nowhere, and the recipient's "the link does not work" looked like a
// spam filter or a slow server rather than what it was. FrontendBaseURL adds the
// scheme and honours FRONTEND_DOMAIN, so an invitation now lands where a reset
// link already did.
func invitationLink(base, token string) string {
	return fmt.Sprintf("%s/signup?token=%s", strings.TrimRight(base, "/"), token)
}

// respondInvitation is the one answer both creating and resending give.
//
// It carries the link, so the admin can hand it over themselves, and it says
// whether an email is actually going out. A fresh install cannot send mail until
// somebody adds a key, and this endpoint used to answer "invitation sent
// successfully" regardless, so the first thing a new admin did after setting up
// was invite a colleague and then wait for an email that was never going to
// come. The link is theirs to share either way: it is their invitation, and a
// mail client's spam folder is a reason to want it even when sending works.
func respondInvitation(w http.ResponseWriter, resent bool, link string) {
	sent := emailService.IsEmailEnabled()
	msg := "Invitation created. Email is not set up on this server, so share the link yourself."
	if sent && resent {
		msg = "Invitation resent. The link below is the same one in the email."
	} else if sent {
		msg = "Invitation sent. The link below is the same one in the email."
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status":      "success",
		"msg":         msg,
		"invite_link": link,
		"email_sent":  sent,
	})
}

func sendInvitationEmail(toEmail string, token string) {
	ctx := context.Background()

	signupLink := invitationLink(authService.FrontendBaseURL(), token)

	// 2. Evaluate backend domain
	backendDomain := strings.TrimRight(os.Getenv("BACKEND_DOMAIN"), "/")
	if backendDomain != "" && !strings.HasPrefix(backendDomain, "http") {
		// Assuming https for prod, http for localhost
		if strings.Contains(backendDomain, "localhost") {
			backendDomain = "http://" + backendDomain
		} else {
			backendDomain = "https://" + backendDomain
		}
	} else if backendDomain == "" {
		backendDomain = "http://localhost:3000" // absolute fallback
	}
	logoImageHtml := fmt.Sprintf(`<img src="%s/public/email/logo" alt="Logo" style="max-height:80px; max-width:200px;" />`, backendDomain)

	// 3. Get email config with robust fallbacks
	senderEmail := fmt.Sprintf("noreply@%s", os.Getenv("FE_DOMAIN"))
	subject := "You're invited to OneCamp!"
	template := `<h2>Welcome to OneCamp!</h2>
{{logo_image}}
<p>You've been invited to join. Click the link below to set up your account:</p>
<p><a href="{{signup_link}}">Accept Invitation</a></p>
<p>This link expires in 7 days.</p>`

	if cfg, err := configModels.GetConfigByKey("sender_email"); err == nil && cfg != nil && cfg.Value != "" {
		senderEmail = cfg.Value
	}
	if cfg, err := configModels.GetConfigByKey("invitation_email_subject"); err == nil && cfg != nil && cfg.Value != "" {
		subject = cfg.Value
	}
	if cfg, err := configModels.GetConfigByKey("invitation_email_template"); err == nil && cfg != nil && cfg.Value != "" {
		template = cfg.Value
	}

	// 4. Inject logo if available
	logoCfg, err := configModels.GetConfigByKey("invitation_email_logo")
	if err != nil || logoCfg == nil || logoCfg.Value == "" {
		logoImageHtml = "" // if no logo config, remove the placeholder completely
	}

	// 5. Replace placeholders
	finalTemplate := strings.ReplaceAll(template, "{{signup_link}}", signupLink)
	finalTemplate = strings.ReplaceAll(finalTemplate, "{{logo_image}}", logoImageHtml)

	_ = emailService.SendInvitationEmail(ctx, toEmail, senderEmail, subject, finalTemplate, signupLink)
}

func GetExternalUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

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

	const maxPageSize = 100
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageIndex < 0 {
		pageIndex = 0
	}

	usersInfo, hasMore, err := business.GetExternalUsers(ctx, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/GetExternalUsers Failed to get external users err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to get external users",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":      "Got external users",
		"data":     usersInfo,
		"has_more": hasMore,
	})
}

func UnlinkExternalUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		UserUUID string `json:"user_uuid"`
	}

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnlinkExternalUser Failed to parse body err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Failed to parse request body",
			"status": "failed",
		})
		return
	}

	userUUID, err := uuid.Parse(input.UserUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnlinkExternalUser Failed to parse userUUID err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID",
			"err": err,
		})
		return
	}

	ok, err := business.UnlinkExternalUser(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/UnlinkExternalUser Failed to unlink user err: %+v",
			err)
		statusCode := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			statusCode = http.StatusNotFound
		}
		helpers.WriteJSON(w, statusCode, helpers.Envolope{
			"msg": err.Error(),
		})
		return
	}

	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "User not found or not an external user",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "Unlinked external user",
		"status": "success",
	})
}
