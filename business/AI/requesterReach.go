package business

// What the person an agent run acts for can see, for the reads that are
// narrowed by a scope rather than checked one object at a time.
//
// An agent's tools run as its sponsor. When someone else asked for the run
// (see services/AI runRequester.go), a read by id is refused up front by the
// agent runner if the asker could not make it themselves; that check needs no
// help from here. What it cannot cover is a read that ANSWERS WITH A LIST: a
// search, "list the projects", "list my tasks". Those take no id to check, so
// they have to be narrowed as they are built, to what the asker can also see.
// This file resolves that once, from the same lookups the rest of the app uses
// for membership, and the list and search paths ask it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	dataSourceBusiness "github.com/akashc777/OneCamp/business/DataSource"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func init() {
	ai.RegisterReaderScopeResolver(readerScopeFor)
}

// errAskerUnknown is the refusal for a run whose asker could not be identified.
// Never a fallback to the sponsor's reach: not knowing who asked is exactly
// the case in which the sponsor's reach must not be lent out.
var errAskerUnknown = errors.New("I couldn't tell who asked for this, so I didn't look it up")

// askerWords puts the authorizer's reasons, which name "the originating person"
// (the MCP surface's term for the human behind a call), in the words a tool
// result relays to the person it means.
var askerWords = strings.NewReplacer(
	"the originating person's account", "the account of the person who asked",
	"the originating person", "the person who asked",
).Replace

// personView is what one person can see, as sets of ids.
type personView struct {
	userUUID string
	channels map[string]bool
	projects map[string]bool
	teams    map[string]bool
	grpIDs   map[string]bool
}

// Seams, so the narrowing rules are tested without a graph.
var (
	lookupPerson         = userDomain.GetDgraphUserInfoByUUID
	lookupPersonProfile  = userDomain.GetActiveDgraphUserInfoByUUID
	lookupPersonByUserID = getUserInfoForExecutor
)

// viewOf resolves what userUUID can see, once per run (services/AI RunMemo):
// a search alone asks it of the asker for every index query it makes.
//
// Eligibility first, from the same rule the MCP authorizer and the delegation
// guard use (principalBusiness.Assess): deactivating someone leaves their
// membership edges in place, so a membership list alone would still answer for
// a person who has left. Then the profile that carries their channels,
// projects, teams and conversations.
func viewOf(ctx context.Context, userUUID string) (*personView, error) {
	return ai.RunMemo(ctx, "view:"+strings.TrimSpace(userUUID), func() (*personView, error) {
		return resolveView(ctx, userUUID)
	})
}

// resolveView is viewOf without the memo.
func resolveView(ctx context.Context, userUUID string) (*personView, error) {
	profile, err := profileOf(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	v := &personView{
		userUUID: userUUID,
		channels: map[string]bool{},
		projects: map[string]bool{},
		teams:    map[string]bool{},
		grpIDs:   map[string]bool{},
	}
	for _, ch := range profile.Channels {
		if ch != nil && ch.Uuid != "" {
			v.channels[ch.Uuid] = true
		}
	}
	for _, p := range profile.Projects {
		if p != nil && p.Uuid != "" {
			v.projects[p.Uuid] = true
		}
	}
	for _, t := range profile.Teams {
		if t != nil && t.Uuid != "" {
			v.teams[t.Uuid] = true
		}
	}
	for _, dm := range profile.DMs {
		if dm != nil && dm.GroupingId != "" {
			v.grpIDs[dm.GroupingId] = true
		}
	}
	return v, nil
}

// profileOf resolves the profile (channels, projects, teams, conversations) of
// a person who may authorize work, refusing anyone who may not.
func profileOf(ctx context.Context, userUUID string) (*dgraphStruct.DgraphUser, error) {
	userUUID = strings.TrimSpace(userUUID)
	if userUUID == "" {
		return nil, errAskerUnknown
	}
	u, err := lookupPerson(ctx, userUUID)
	if err != nil || u == nil {
		return nil, fmt.Errorf("the person who asked could not be resolved")
	}
	if e := principalBusiness.Assess(u); !e.Allowed {
		return nil, errors.New(askerWords(e.Reason))
	}
	profile, err := lookupPersonProfile(ctx, userUUID)
	if err != nil || profile == nil {
		return nil, fmt.Errorf("could not read what the person who asked can see")
	}
	return profile, nil
}

// askerView returns what the person this run acts for can see, or nil when
// the run acts for its sponsor alone (nobody else asked), in which case
// nothing is narrowed.
func askerView(ctx context.Context) (*personView, error) {
	requester, _, ok := ai.RunRequester(ctx)
	if !ok {
		return nil, nil
	}
	return viewOf(ctx, requester)
}

// readerScopeFor is the semantic index's view of a person (registered with
// services/AI at init).
func readerScopeFor(ctx context.Context, userUUID string) (ai.ReaderScope, error) {
	v, err := viewOf(ctx, userUUID)
	if err != nil {
		return ai.ReaderScope{}, err
	}
	return ai.ReaderScope{
		Channels: setKeys(v.channels),
		Projects: setKeys(v.projects),
		GrpIDs:   setKeys(v.grpIDs),
	}, nil
}

// askerTableActor is the person the run acts for as the tables know them, or
// nil when the run acts for its sponsor alone: their own Postgres identity,
// where the admin flag the tables' rule tests is authoritative.
func askerTableActor(ctx context.Context) (*dataTableBusiness.Actor, error) {
	requester, _, ok := ai.RunRequester(ctx)
	if !ok {
		return nil, nil
	}
	if strings.TrimSpace(requester) == "" {
		return nil, errAskerUnknown
	}
	info, err := askerInfo(ctx, requester)
	if err != nil {
		return nil, err
	}
	return &dataTableBusiness.Actor{UserID: info.UserPostgresInfo.Id, IsAdmin: info.UserPostgresInfo.IsAdmin}, nil
}

// askerInfo is the person who asked as the executors know a user, once per run.
func askerInfo(ctx context.Context, requester string) (*userModels.UserInfo, error) {
	return ai.RunMemo(ctx, "user:"+requester, func() (*userModels.UserInfo, error) {
		info, err := lookupPersonByUserID(ctx, requester)
		if err != nil || info == nil {
			return nil, fmt.Errorf("the person who asked could not be resolved")
		}
		return info, nil
	})
}

// forAskerToo has the table reads and writes made with the returned context
// open only tables the person the run acts for can open as well
// (dataTableBusiness.AlsoFor): the links and rollups of a table they may read
// reach into other tables, which the sponsor alone may be able to open. ctx
// comes back as it was for a run that acts for its sponsor alone.
func forAskerToo(ctx context.Context) (context.Context, error) {
	asker, err := askerTableActor(ctx)
	if err != nil || asker == nil {
		return ctx, err
	}
	return dataTableBusiness.AlsoFor(ctx, *asker), nil
}

// askerTables returns the ids of the tables the asker may view, or nil when the
// run acts for its sponsor alone. Same rule as reading one (dataTableBusiness).
func askerTables(ctx context.Context) (map[string]bool, error) {
	asker, err := askerTableActor(ctx)
	if err != nil || asker == nil {
		return nil, err
	}
	tables, err := dataTableBusiness.ListTables(ctx, *asker)
	if err != nil {
		return nil, fmt.Errorf("could not read which tables the person who asked can see")
	}
	out := make(map[string]bool, len(tables))
	for _, t := range tables {
		if t != nil {
			out[t.Id.String()] = true
		}
	}
	return out, nil
}

// askerDataSources is askerTables for external data sources: the ids the asker
// may query, by the same per-source rule (dataSourceBusiness.ListQueryable).
func askerDataSources(ctx context.Context) (map[string]bool, error) {
	requester, _, ok := ai.RunRequester(ctx)
	if !ok {
		return nil, nil
	}
	if strings.TrimSpace(requester) == "" {
		return nil, errAskerUnknown
	}
	info, err := askerInfo(ctx, requester)
	if err != nil {
		return nil, err
	}
	items, err := dataSourceBusiness.ListQueryable(ctx, dataSourceBusiness.Actor{UserID: info.UserPostgresInfo.Id, IsAdmin: info.UserPostgresInfo.IsAdmin})
	if err != nil {
		return nil, fmt.Errorf("could not read which data sources the person who asked can query")
	}
	out := make(map[string]bool, len(items))
	for _, d := range items {
		out[d.Id] = true
	}
	return out, nil
}

// setKeys lists a set's members.
func setKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
