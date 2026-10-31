package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	globalSearch "github.com/akashc777/OneCamp/business/GlobalSearch"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	business "github.com/akashc777/OneCamp/business/SearchQuery"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// searchesPerMinute is how many searches one person's field may run a
// minute: one after each pause in typing.
const searchesPerMinute = 300

// allowSearch is the person's window, a seam for tests.
var allowSearch = func(ctx context.Context, userUUID string) (bool, int) {
	res := redisStore.AllowFixedWindowOrLocal(ctx, registry.QuerySearchRate, []string{userUUID}, searchesPerMinute)
	return res.Allowed, res.RetryAfterSeconds()
}

// runSearch is the engine, a seam for tests.
var runSearch = globalSearch.Search

type searchRequest struct {
	// Q is the text, in the one grammar.
	Q string `json:"q"`
	// Kinds are the kinds this page lists (a kind's chip); the counts still
	// count every kind.
	Kinds []string `json:"kinds,omitempty"`
	// Sort is best (the default), newest or oldest.
	Sort string `json:"sort,omitempty"`
	// After is the cursor a page answered with.
	After string `json:"after,omitempty"`
	Size  int    `json:"size,omitempty"`
	// Zone is the asker's time zone (IANA), where dates are counted; UTC
	// when it's missing or unknown.
	Zone string `json:"zone,omitempty"`
	// One conversation, from its header (QA F-15): a channel by its id, a DM
	// by the other person's, a group by its own. The asker's reach still
	// holds: one they aren't in finds nothing.
	Channel string `json:"channel,omitempty"`
	DM      string `json:"dm,omitempty"`
	Group   string `json:"group,omitempty"`
}

// within is the one conversation the request names, if any.
func (req searchRequest) within(asker string) globalSearch.Within {
	w := globalSearch.Within{ChannelUUID: strings.TrimSpace(req.Channel), GroupingID: strings.TrimSpace(req.Group)}
	if dm := strings.TrimSpace(req.DM); dm != "" {
		w.GroupingID = helpers.GetGroupingId(asker, dm)
	}
	return w
}

var knownKinds = map[string]bool{
	globalSearch.KindMessage: true, globalSearch.KindDoc: true, globalSearch.KindTask: true, globalSearch.KindProject: true,
	globalSearch.KindFile: true, globalSearch.KindPerson: true, globalSearch.KindChannel: true, globalSearch.KindBoard: true,
	globalSearch.KindTeam: true,
}

type searchAnswer struct {
	globalSearch.Result
	// Query is the text read: the spec, its issues, its tokens and its
	// canonical text, so chips and "searched as words" come with the hits.
	Query parseAnswer `json:"query"`
}

// SearchQuery POST /query/search {q, kinds?, sort?, after?, size?, zone?, channel? | dm? | group?}
// searches the workspace as the signed-in person with the one grammar: the
// page of hits, how many match in each kind and in all, the cursor of the
// next page, notes on what couldn't be applied, and the query as read. A
// text that can't run (past a limit) is still a 200, with no hits and the
// issue that says why.
func SearchQuery(w http.ResponseWriter, r *http.Request) {
	user, subject := accessBusiness.RequestSubject(r)
	if subject.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return
	}
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the search as JSON: {\"q\": \"…\"}."})
		return
	}
	for _, k := range req.Kinds {
		if !knownKinds[k] {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "There's no kind of thing called \"" + k + "\" to search."})
			return
		}
	}
	switch req.Sort {
	case "", globalSearch.SortBest, globalSearch.SortNewest, globalSearch.SortOldest:
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A search is sorted by best, newest or oldest."})
		return
	}
	if req.Size < 0 || req.Size > globalSearch.MaxPageSize {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A page holds 1 to " + strconv.Itoa(globalSearch.MaxPageSize) + " results."})
		return
	}
	if ok, retry := allowSearch(r.Context(), subject.UserUUID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Too many searches in a minute. Wait a moment and try again.", "status": "rate_limited"})
		return
	}

	who := business.Asker{Subject: subject, Email: user.UserPostgresInfo.EmailID}
	spec, issues := business.Build(r.Context(), who, req.Q, querySpec.Scope{Kind: querySpec.ScopeSearch}, business.ModeSearch)
	answer := searchAnswer{
		Result: globalSearch.Result{Totals: map[string]int{}},
		Query: parseAnswer{
			Spec: spec, Runs: business.Runs(issues), Issues: issues,
			Tokens: business.Parse(req.Q).Tokens, Text: business.Format(r.Context(), who, spec),
		},
	}
	if answer.Query.Issues == nil {
		answer.Query.Issues = []business.Issue{}
	}
	if answer.Query.Tokens == nil {
		answer.Query.Tokens = []business.Token{}
	}
	if answer.Query.Runs && strings.TrimSpace(req.Q) != "" {
		reach := globalSearch.ReachOf(subject, user.UserPostgresInfo.EmailID,
			user.UserDgraphInfo.Channels, user.UserDgraphInfo.Projects, user.UserDgraphInfo.Teams)
		narrowed := spec
		narrowed.Filter = req.within(subject.UserUUID).Narrow(spec.Filter)
		result, err := runSearch(r.Context(), reach, narrowed, globalSearch.Page{
			Size: req.Size, After: req.After, Kinds: req.Kinds, Sort: req.Sort, Now: time.Now(), Zone: zoneOf(req.Zone),
		})
		if errors.Is(err, globalSearch.ErrBadSearchCursor) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That page of results can't be found. Search again."})
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(r.Context(), "controllers/SearchQuery: the search failed: %+v", err)
			helpers.WriteRetryLater(w, "Search couldn't be reached.")
			return
		}
		answer.Result = result
	}
	if answer.Page == nil {
		answer.Page = []*openSearchStruct.GlobalSearchOpenSearchResp{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": answer})
}

// zoneOf is an IANA zone's location, or UTC.
func zoneOf(name string) *time.Location {
	if name = strings.TrimSpace(name); name == "" || len(name) > 64 {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
