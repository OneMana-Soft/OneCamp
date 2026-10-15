package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"

	domain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

type GlobalSearchPagination struct {
	Page    []*openSearchStruct.GlobalSearchOpenSearchResp `json:"page,omitempty"`
	HasMore bool                                           `json:"has_more"`
	// Next is the cursor of the page after this one, while HasMore: what the
	// search page's "Show more" sends back as after.
	Next string `json:"next,omitempty"`
}

// ErrBadSearchCursor is a cursor this server didn't write.
var ErrBadSearchCursor = errors.New("that isn't a search cursor")

// encodeSearchCursor writes the sort values of a page's last hit (its score,
// then its time) as an opaque cursor; "" when there are none.
func encodeSearchCursor(sortValues []any) string {
	if len(sortValues) == 0 {
		return ""
	}
	raw, err := json.Marshal(sortValues)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeSearchCursor reads a cursor encodeSearchCursor wrote: two finite
// numbers, a score and a time. Anything else is ErrBadSearchCursor; the
// values go into the query marshalled, never as text.
func DecodeSearchCursor(cursor string) ([]float64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 128 {
		return nil, ErrBadSearchCursor
	}
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil || len(values) != 2 {
		return nil, ErrBadSearchCursor
	}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, ErrBadSearchCursor
		}
	}
	return values, nil
}

// GetUnifiedGlobalSearch is the first page of the unified search.
func GetUnifiedGlobalSearch(ctx context.Context, userUUID string, userEmail string, userChannels []*dgraphStruct.DgraphChannel, userProjects []*dgraphStruct.DgraphProject, userTeams []*dgraphStruct.DgraphTeam, searchText string) (searchPage GlobalSearchPagination, err error) {
	return GetUnifiedGlobalSearchPage(ctx, userUUID, userEmail, userChannels, userProjects, userTeams, searchText, nil)
}

// GetUnifiedGlobalSearchPage is a page of the unified search: the first with
// no after, else the one after the cursor a page answered with (Next). The
// search page counted its 20 hits as "20 results" and never asked for more.
func GetUnifiedGlobalSearchPage(ctx context.Context, userUUID string, userEmail string, userChannels []*dgraphStruct.DgraphChannel, userProjects []*dgraphStruct.DgraphProject, userTeams []*dgraphStruct.DgraphTeam, searchText string, after []float64) (searchPage GlobalSearchPagination, err error) {
	var channelUUIDs []string
	for _, ch := range userChannels {
		channelUUIDs = append(channelUUIDs, ch.Uuid)
	}

	projectUUIDMap := make(map[string]bool)
	for _, proj := range userProjects {
		projectUUIDMap[proj.Uuid] = true
	}

	for _, team := range userTeams {
		for _, proj := range team.Projects {
			projectUUIDMap[proj.Uuid] = true
		}
	}

	var projectUUIDs []string
	for uuid := range projectUUIDMap {
		projectUUIDs = append(projectUUIDs, uuid)
	}

	var teamUUIDs []string
	for _, team := range userTeams {
		teamUUIDs = append(teamUUIDs, team.Uuid)
	}

	searchInfo, err := domain.GetUnifiedGlobalSearchFromOpenSearch(ctx, userUUID, userEmail, channelUUIDs, projectUUIDs, teamUUIDs, searchText, after)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUnifiedGlobalSearch Failed to get unified search from opensearch err: %+v",
			err)

		return
	}

	searchPage = page(ctx, searchInfo, domain.UNIFIED_SEARCH_COUNT)
	// The next page starts after the last hit the index gave this one,
	// whether or not a check below leaves it out.
	if searchPage.HasMore {
		searchPage.Next = encodeSearchCursor(searchInfo[domain.UNIFIED_SEARCH_COUNT-1].Sort)
	}
	// After the page is cut: whether there is more is what the index said.
	searchPage.Page = keepReadableDocHits(ctx, userUUID, searchPage.Page)
	searchPage.Page = keepReadableBoardHits(ctx, userUUID, searchPage.Page)
	searchPage.Page = keepReadableProjectFileHits(ctx, userUUID, userProjects, searchPage.Page)

	return
}
