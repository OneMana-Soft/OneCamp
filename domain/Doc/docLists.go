package domain

// The doc lists the docs home shows: Mine, Shared with me, Workspace and
// Archived, read here with one exact count under the same filter as the page.
// (Recent and Starred are kept in Postgres, and their docs read by uuid:
// GetDocRowsByUUID.)
//
// Each scope picks docs by how the person reaches them, which today's access
// rule (business/Doc access.go) reads the same way: a private doc is theirs or
// shared with them through an edge to them, a public one is everyone's. The
// rows still carry what the rule reads, and business/Doc asks it of every row
// before naming one, so a list never names a doc the rule says no to.

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
)

// List scopes read from the graph.
const (
	ScopeMine      = "mine"      // docs the person owns
	ScopeShared    = "shared"    // docs shared with them that they don't own
	ScopeWorkspace = "workspace" // docs everyone in the workspace can open
	ScopeArchived  = "archived"  // docs the retention archived that were theirs to open
)

// Sort orders of a list.
const (
	SortEdited  = "edited"
	SortCreated = "created"
	SortTitle   = "title"
)

// DocListQuery is one page of one list.
type DocListQuery struct {
	Scope     string
	ViewerUID string
	Sort      string
	// OwnerUUID keeps only docs this person owns.
	OwnerUUID string
	// SpaceKind and SpaceUUID keep only the docs of one space.
	SpaceKind, SpaceUUID string
	First, Offset        int
}

// ListDocs reads one page of a list and how many docs the whole list holds.
func ListDocs(ctx context.Context, q DocListQuery) (*dgraphStruct.DgraphDocList, error) {
	query, vars, err := docListRequest(q)
	if err != nil {
		return nil, err
	}
	list, err := dgraphModels.GetDgraphDocsWithCount(ctx, query, vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ListDocs %s err: %+v", q.Scope, err)
	}
	return list, err
}

// docListRequest is the query for one page of a list. Pure. It names only
// the variables its scope uses: the graph refuses a query that defines one
// and never reads it.
func docListRequest(q DocListQuery) (string, map[string]string, error) {
	vars := map[string]string{"$viewer": q.ViewerUID}
	params := []string{"$viewer: string"}
	filters := []string{notInternalFilter}
	var cands string
	var reach []string // how the person reaches the docs the scope reads
	teams := false     // and through the teams they're in
	order := "orderdesc: doc_updated_at"
	switch q.Scope {
	case ScopeMine:
		reach = []string{"owned", "made"}
		cands = "uid(owned, made)"
		filters = append(filters, liveDocFilter, "(uid(owned) OR NOT has(doc_owner))")
	case ScopeShared:
		reach = []string{"owned", "made", "reading", "editing", "commenting", "full"}
		teams = true
		cands = "uid(reading, editing, commenting, full, teamReading, teamCommenting, teamEditing)"
		filters = append(filters, liveDocFilter, "NOT uid(owned)", "NOT (uid(made) AND NOT has(doc_owner))")
	case ScopeWorkspace:
		cands = "eq(doc_private, false)"
		filters = append(filters, liveDocFilter)
	case ScopeArchived:
		reach = []string{"owned", "made", "reading", "editing", "commenting", "full"}
		teams = true
		cands = "uid(owned, made, reading, editing, commenting, full, teamReading, teamCommenting, teamEditing, everyones)"
		filters = append(filters, archivedDocFilter, deletedDocFilter)
		order = "orderdesc: doc_archived_at"
	default:
		return "", nil, fmt.Errorf("domain/ListDocs: no scope %q", q.Scope)
	}
	switch q.Sort {
	case SortCreated:
		order = "orderdesc: doc_created_at"
	case SortTitle:
		order = "orderasc: doc_title"
	case SortEdited:
		if q.Scope != ScopeArchived {
			order = "orderdesc: doc_updated_at"
		}
	}

	var blocks strings.Builder
	if len(reach) > 0 {
		edges := map[string]string{
			"owned": "~doc_owner", "made": "~doc_created_by", "reading": "~doc_reading_users",
			"editing": "~doc_editing_users", "commenting": "~doc_commenting_users", "full": "~doc_full_access_users",
		}
		blocks.WriteString("\n\t\tvar(func: uid($viewer)) {")
		for _, v := range reach {
			fmt.Fprintf(&blocks, "\n\t\t\t%s as %s", v, edges[v])
		}
		blocks.WriteString("\n\t\t}")
	}
	if teams {
		// Docs shared with a team the person is in (AccessPrelude's teams).
		blocks.WriteString("\n\t\tvar(func: uid(accTm, accTa)) @filter(" + liveTeamFilter + ") {" +
			"\n\t\t\tteamReading as ~doc_team_viewers\n\t\t\tteamCommenting as ~doc_team_commenters\n\t\t\tteamEditing as ~doc_team_editors\n\t\t}")
	}
	if q.Scope == ScopeArchived {
		blocks.WriteString("\n\t\teveryones as var(func: eq(doc_private, false)) @filter(" + archivedDocFilter + ")")
	}
	if q.OwnerUUID != "" {
		params = append(params, "$owner: string")
		vars["$owner"] = q.OwnerUUID
		blocks.WriteString(`
		var(func: eq(user_uuid, $owner)) {
			theirs as ~doc_owner
			theyMade as ~doc_created_by
		}`)
		filters = append(filters, "(uid(theirs) OR (uid(theyMade) AND NOT has(doc_owner)))")
	}
	if q.SpaceUUID != "" {
		params = append(params, "$skind: string", "$suuid: string")
		vars["$skind"], vars["$suuid"] = q.SpaceKind, q.SpaceUUID
		filters = append(filters, "eq(doc_space_kind, $skind)", "eq(doc_space_uuid, $suuid)")
	}
	first, offset := q.First, q.Offset
	if first < 1 {
		first = 1
	}
	if offset < 0 {
		offset = 0
	}
	query := fmt.Sprintf(`query list(%s) {`+AccessPrelude("$viewer")+`%s
		cands as var(func: %s) @filter(%s)
		doc_count(func: uid(cands)) {
			count: count(uid)
		}
		docInfo(func: uid(cands), %s, first: %d, offset: %d) {%s%s
		}
	}`, strings.Join(params, ", "), blocks.String(), cands, strings.Join(filters, " AND "), order, first, offset, docRowFields, docPathFields(docListPathDepth))
	return query, vars, nil
}
