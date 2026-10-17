package controllers

import (
	"encoding/json"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/GlobalSearch"
	business "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

func GetUnifiedGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var searchInfo adapter.GlobalSearchInfo

	if r.Method == http.MethodGet {
		searchInfo.SearchText = chi.URLParam(r, "search_text")
		searchInfo.After = r.URL.Query().Get("after")
		searchInfo.Channel = r.URL.Query().Get("channel")
		searchInfo.DM = r.URL.Query().Get("dm")
		searchInfo.Group = r.URL.Query().Get("group")
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

	// A later page: after the cursor the page before answered with.
	var after []float64
	if searchInfo.After != "" {
		var cursorErr error
		if after, cursorErr = business.DecodeSearchCursor(searchInfo.After); cursorErr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That page of results can't be found. Search again."})
			return
		}
	}

	// Search in one conversation, from its header (QA F-15): a channel by its
	// id, a DM by the other person's, a group by its own.
	within := business.Within{ChannelUUID: strings.TrimSpace(searchInfo.Channel), GroupingID: strings.TrimSpace(searchInfo.Group)}
	if dm := strings.TrimSpace(searchInfo.DM); dm != "" {
		within.GroupingID = helpers.GetGroupingId(userInfo.UserDgraphInfo.Uuid, dm)
	}

	searchPage, err := business.GetUnifiedGlobalSearchPage(ctx, userInfo.UserDgraphInfo.Uuid, userInfo.UserPostgresInfo.EmailID, userInfo.UserDgraphInfo.Channels, userInfo.UserDgraphInfo.Projects, userInfo.UserDgraphInfo.Teams, searchInfo.SearchText, after, within)

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
