package business

import (
	"context"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// GlobalSearchPagination is a page of the search box's search, for the
// callers that send its text as it was typed: the palette, the pickers,
// /v1/search and the assistant's named search.
type GlobalSearchPagination struct {
	Page    []*openSearchStruct.GlobalSearchOpenSearchResp `json:"page,omitempty"`
	HasMore bool                                           `json:"has_more"`
	// Next is the cursor of the page after this one, while HasMore: what the
	// search page's "Show more" sends back as after.
	Next string `json:"next,omitempty"`
	// Totals are how many match in each kind; Total in all.
	Totals map[string]int `json:"totals,omitempty"`
	Total  int            `json:"total"`
}

// GetUnifiedGlobalSearch is the first page of the unified search.
func GetUnifiedGlobalSearch(ctx context.Context, who accessBusiness.Subject, userEmail string, userChannels []*dgraphStruct.DgraphChannel, userProjects []*dgraphStruct.DgraphProject, userTeams []*dgraphStruct.DgraphTeam, searchText string) (searchPage GlobalSearchPagination, err error) {
	return GetUnifiedGlobalSearchPage(ctx, who, userEmail, userChannels, userProjects, userTeams, searchText, "", Within{})
}

// Within narrows a search to one conversation: a channel by its id, or a DM
// or a group by its grouping id (helpers.GetGroupingId for a DM). The zero
// value searches everything the reader may see. A conversation the reader
// isn't in finds nothing: the reader's own reach still holds.
type Within struct {
	ChannelUUID string
	GroupingID  string
}

// ReachOf is what someone (who) reaches through membership, from the lists
// their sign-in carries: their channels, projects and teams. Only the
// projects they are in themselves: a project of a team they're in isn't
// theirs to search unless they're in it too.
func ReachOf(who accessBusiness.Subject, userEmail string, channels []*dgraphStruct.DgraphChannel, projects []*dgraphStruct.DgraphProject, teams []*dgraphStruct.DgraphTeam) Reach {
	r := Reach{Who: who, UserUUID: who.UserUUID, Email: userEmail}
	for _, ch := range channels {
		if ch != nil && ch.Uuid != "" {
			r.Channels = append(r.Channels, ch.Uuid)
		}
	}
	seen := map[string]bool{}
	for _, p := range projects {
		if p != nil && p.Uuid != "" && !seen[p.Uuid] {
			seen[p.Uuid] = true
			r.Projects = append(r.Projects, p.Uuid)
		}
	}
	for _, t := range teams {
		if t != nil && t.Uuid != "" {
			r.Teams = append(r.Teams, t.Uuid)
		}
	}
	return r
}

// GetUnifiedGlobalSearchPage is a page of the unified search: the first with
// no after, else the one after the cursor a page answered with (Next). The
// text is read with the one grammar as a search (business/SearchQuery), so a
// modifier typed here filters too, and anything it can't read is searched as
// the words typed. who is the person searching, as the caller names them for
// the Access Resolver (accessBusiness.RequestSubject for a request): a place
// named in the text is found by it, so an asker built from a uuid alone, with
// no graph uid, found no team.
func GetUnifiedGlobalSearchPage(ctx context.Context, who accessBusiness.Subject, userEmail string, userChannels []*dgraphStruct.DgraphChannel, userProjects []*dgraphStruct.DgraphProject, userTeams []*dgraphStruct.DgraphTeam, searchText string, after string, within Within) (searchPage GlobalSearchPagination, err error) {
	asker := searchQuery.Asker{Subject: who, Email: userEmail}
	spec, issues := searchQuery.Build(ctx, asker, searchText, querySpec.Scope{Kind: querySpec.ScopeSearch}, searchQuery.ModeSearch)
	if !searchQuery.Runs(issues) {
		return GlobalSearchPagination{}, nil
	}
	spec.Filter = within.Narrow(spec.Filter)
	result, err := Search(ctx, ReachOf(who, userEmail, userChannels, userProjects, userTeams), spec, Page{After: after, Now: time.Now()})
	if err != nil {
		return GlobalSearchPagination{}, err
	}
	return GlobalSearchPagination{Page: result.Page, HasMore: result.HasMore, Next: result.Next, Totals: result.Totals, Total: result.Total}, nil
}

// Narrow is f narrowed to the one conversation, or f for the zero value.
func (within Within) Narrow(f *querySpec.Filter) *querySpec.Filter {
	var place querySpec.Value
	switch {
	case within.ChannelUUID != "":
		place = querySpec.Value{ID: within.ChannelUUID, Type: "channel"}
	case within.GroupingID != "":
		place = querySpec.Value{ID: within.GroupingID, Type: placeConversation}
	default:
		return f
	}
	in := querySpec.Filter{Field: "in", Operator: querySpec.OperatorIs, Values: []querySpec.Value{place}}
	if f == nil {
		return &in
	}
	return &querySpec.Filter{Op: querySpec.OpAnd, Filters: []querySpec.Filter{*f, in}}
}

// demoPeopleHits returns hits without the people the public demo keeps from
// its shared visitor (helpers.DemoListsPerson): only the demo's own people
// are found. Every other hit, and every hit for anyone else, stays.
func demoPeopleHits(viewerEmail string, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	if !helpers.IsDemoVisitor(viewerEmail) {
		return hits
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	for _, h := range hits {
		if h != nil && h.User != nil && !helpers.DemoListsPerson(viewerEmail, h.User.UserEmail, false) {
			continue
		}
		kept = append(kept, h)
	}
	return kept
}
