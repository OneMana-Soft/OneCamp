package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/GlobalSearch"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

func GetUnifiedGlobalSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The signed-in person, as every handler that asks the Access Resolver
	// names them: the search reads a place named in its text as them.
	user, who := accessBusiness.RequestSubject(r)
	if who.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return
	}

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

	// Search in one conversation, from its header (QA F-15): a channel by its
	// id, a DM by the other person's, a group by its own.
	within := business.Within{ChannelUUID: strings.TrimSpace(searchInfo.Channel), GroupingID: strings.TrimSpace(searchInfo.Group)}
	if dm := strings.TrimSpace(searchInfo.DM); dm != "" {
		within.GroupingID = helpers.GetGroupingId(who.UserUUID, dm)
	}

	// A later page: after the cursor the page before answered with.
	searchPage, err := business.GetUnifiedGlobalSearchPage(ctx, who, user.UserPostgresInfo.EmailID, user.UserDgraphInfo.Channels, user.UserDgraphInfo.Projects, user.UserDgraphInfo.Teams, searchInfo.SearchText, searchInfo.After, within)
	if errors.Is(err, business.ErrBadSearchCursor) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That page of results can't be found. Search again."})
		return
	}
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
