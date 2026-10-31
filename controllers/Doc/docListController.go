package docController

// The docs home's lists (router/docsLists.go) and the trash
// (router/docsTrash.go). Each list answers one page and how many it holds in
// all, under the same filter (B11): Recent and Starred from what the person
// did, Mine, Shared with me and Workspace from the graph, and Archived from
// the retention archive. A row's path names only the pages above it the
// person can open.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// List scopes kept outside the graph.
const (
	scopeRecent  = "recent"
	scopeStarred = "starred"
)

// ListItem is one row of a doc list.
type ListItem struct {
	UUID       string           `json:"doc_uuid"`
	Title      string           `json:"doc_title"`
	Icon       string           `json:"doc_icon,omitempty"`
	Snippet    string           `json:"doc_snippet,omitempty"`
	Private    *bool            `json:"doc_private,omitempty"`
	CreatedAt  *time.Time       `json:"doc_created_at,omitempty"`
	UpdatedAt  *time.Time       `json:"doc_updated_at,omitempty"`
	ArchivedAt *time.Time       `json:"doc_archived_at,omitempty"`
	Owner      *business.Person `json:"owner,omitempty"`
	UpdatedBy  *business.Person `json:"updated_by,omitempty"` // who made the change at doc_updated_at (docs F1), never the owner in its place
	Path       []business.Crumb `json:"path"`
	ViewedAt   *time.Time       `json:"viewed_at,omitempty"`
	// Level is what the person may do with it, by the Access Resolver
	// ("view", "comment", "edit", "full"): the list marks one they can't
	// change (docs incident, 14 Oct 2026). Left out for an archived doc.
	Level string `json:"level,omitempty"`
	// What a node that isn't a page is (docs F1): folder, file or link, its
	// colour, a link's address and a file's facts. A page carries none of
	// them.
	NodeKind string              `json:"doc_node_kind,omitempty"`
	Color    string              `json:"doc_color,omitempty"`
	LinkURL  string              `json:"doc_link_url,omitempty"`
	File     *business.FileFacts `json:"doc_file,omitempty"`
}

// readableIDs is which of the docs the person can open now, by the Access
// Resolver.
func readableIDs(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	res := make([]accessBusiness.Resource, len(ids))
	for i, id := range ids {
		res[i] = docRes(id)
	}
	kept, err := accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(kept))
	for _, r := range kept {
		out[r.UUID] = true
	}
	return out, nil
}

// listItems turns rows into list items, naming only the pages above each that
// the person can open, and saying what they may do with each row: one read of
// the Access Resolver for the rows and the pages above them together.
func listItems(ctx context.Context, who accessBusiness.Subject, rows []*dgraphStruct.DgraphDoc) ([]ListItem, error) {
	var ask []accessBusiness.Resource
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ask = append(ask, docRes(id))
		}
	}
	for _, d := range rows {
		add(d.Uuid)
		for _, p := range business.PathAbove(d) {
			add(p.Uuid)
		}
	}
	decided := map[accessBusiness.Resource]accessBusiness.Decision{}
	if len(ask) > 0 {
		got, err := accessBusiness.ResolveAll(ctx, who, ask)
		if err != nil {
			return nil, err
		}
		decided = got
	}
	open := func(id string) bool { return decided[docRes(id)].Level >= accessBusiness.LevelView }
	// The page's files' facts, in one read for them all (docs F1).
	files, err := business.FileFactsOf(ctx, rows)
	if err != nil {
		return nil, err
	}
	items := make([]ListItem, 0, len(rows))
	for _, d := range rows {
		it := ListItem{
			UUID: d.Uuid, Title: d.Title, Icon: d.Icon, Snippet: d.Snippet, Private: d.IsPrivate,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, ViewedAt: d.ViewedAt,
			NodeKind: d.NodeKind, Color: d.Color, LinkURL: d.LinkURL, File: files[strings.ToLower(d.Uuid)],
		}
		if helpers.IsSoftDeleted(d.ArchivedAt) {
			it.ArchivedAt = d.ArchivedAt
		} else if open(d.Uuid) {
			it.Level = decided[docRes(d.Uuid)].Level.String()
		}
		it.Owner = business.OwnerOf(d)
		it.UpdatedBy = business.PersonOf(d.UpdatedBy)
		it.Path = business.CrumbsOf(d, open)
		items = append(items, it)
	}
	return items, nil
}

// GetDocList is one page of a doc list (GET /doc/list?scope=recent|mine|
// shared|workspace|starred|archived&sort=edited|created|title&owner=&space=
// &cursor=&limit=), with the exact count of the whole list.
func GetDocList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	q := r.URL.Query()
	scope, sort := q.Get("scope"), q.Get("sort")
	switch sort {
	case "", domain.SortEdited, domain.SortCreated, domain.SortTitle:
	default:
		badRequest(w, "Sort by edited, created or title.")
		return
	}
	owner := q.Get("owner")
	if owner != "" && !helpers.IsCanonicalUUID(owner) {
		badRequest(w, "That isn't a person.")
		return
	}
	limit, offset := pageLimit(r), helpers.CursorOffset(q.Get("cursor"))

	var rows []*dgraphStruct.DgraphDoc
	var count int
	switch scope {
	case scopeRecent, scopeStarred:
		var all []*dgraphStruct.DgraphDoc
		var err error
		if scope == scopeRecent {
			all, err = business.RecentDocs(ctx, who.UserUUID, who.UserUID, time.Now())
		} else {
			all, err = business.StarredDocs(ctx, who.UserUUID, who.UserUID)
		}
		if err != nil {
			treeFailed(w, r, "GetDocList", err)
			return
		}
		ids := make([]string, len(all))
		for i, d := range all {
			ids[i] = d.Uuid
		}
		open, err := readableIDs(ctx, who, ids)
		if err != nil {
			treeFailed(w, r, "GetDocList", err)
			return
		}
		var kept []*dgraphStruct.DgraphDoc
		for _, d := range all {
			if open[d.Uuid] {
				kept = append(kept, d)
			}
		}
		count = len(kept)
		if offset < len(kept) {
			rows = kept[offset:min(offset+limit, len(kept))]
		}
	case domain.ScopeMine, domain.ScopeShared, domain.ScopeWorkspace, domain.ScopeArchived:
		spaceKind, spaceUUID := "", ""
		if s := q.Get("space"); s != "" {
			if _, k, id := business.TreeSection(s); k != "" {
				spaceKind, spaceUUID = k, id
			}
		}
		list, err := business.ListDocs(ctx, domain.DocListQuery{
			Scope: scope, ViewerUID: who.UserUID, Sort: sort, OwnerUUID: owner,
			SpaceKind: spaceKind, SpaceUUID: spaceUUID, First: limit, Offset: offset,
		})
		if err != nil {
			treeFailed(w, r, "GetDocList", err)
			return
		}
		count = int(list.Count)
		rows, err = guardRows(ctx, who, scope, list.Docs)
		if err != nil {
			treeFailed(w, r, "GetDocList", err)
			return
		}
	default:
		badRequest(w, "Scope is recent, mine, shared, workspace, starred or archived.")
		return
	}
	items, err := listItems(ctx, who, rows)
	if err != nil {
		treeFailed(w, r, "GetDocList", err)
		return
	}
	next := ""
	if offset+limit < count {
		next = helpers.OffsetCursor(offset + limit)
	}
	_ = user
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"scope": scope, "docs": items, "count": count, "next_cursor": next}})
}

// guardRows asks the access rule of every row a graph list read, and drops a
// row it says no to (logging it: the list's query and the rule disagree, and
// the count is then one too many). An archived doc is in the trash as far as
// the Access Resolver is concerned, so Archived asks today's rule for whether
// the person could open it before it was archived.
func guardRows(ctx context.Context, who accessBusiness.Subject, scope string, rows []*dgraphStruct.DgraphDoc) ([]*dgraphStruct.DgraphDoc, error) {
	if scope == domain.ScopeArchived {
		out := rows[:0]
		for _, d := range rows {
			if business.CouldRead(d, who.UserUUID) {
				out = append(out, d)
			}
		}
		return out, nil
	}
	ids := make([]string, len(rows))
	for i, d := range rows {
		ids[i] = d.Uuid
	}
	open, err := readableIDs(ctx, who, ids)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, d := range rows {
		if open[d.Uuid] {
			out = append(out, d)
		} else {
			helpers.LogErrorWithContext(ctx, "controllers/GetDocList: %s listed %s, which the access rule says the person can't open", scope, d.Uuid)
		}
	}
	return out, nil
}

// GetDocTrash is the person's trash (GET /doc/trash?cursor=&limit=): what
// they could change that was deleted lately, newest first, a page deleted
// with its subpages as one entry.
func GetDocTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	items, err := business.Trash(ctx, who.UserUID, who.ActingAdmin(), time.Now())
	if err != nil {
		treeFailed(w, r, "GetDocTrash", err)
		return
	}
	limit, offset := pageLimit(r), helpers.CursorOffset(r.URL.Query().Get("cursor"))
	page := []business.TrashItem{}
	if offset < len(items) {
		page = items[offset:min(offset+limit, len(items))]
	}
	next := ""
	if offset+limit < len(items) {
		next = helpers.OffsetCursor(offset + limit)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"items": page, "count": len(items), "next_cursor": next}})
}

// docRequest is a trash request about one doc. Placed is a restore's: the
// page's block is back in its parent's body already, put there by the
// editor's Undo (docs C7a), so the restore places none.
type docRequest struct {
	DocUUID string `json:"doc_uuid"`
	Placed  bool   `json:"placed"`
}

func readDocRequest(w http.ResponseWriter, r *http.Request, what string) (docRequest, bool) {
	var in docRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Say which doc to "+what+".")
		return in, false
	}
	return in, true
}

// RestoreDocFromTrash brings a doc back (POST /doc/restore): with everything
// deleted with it, or archived under it. A page whose parent is gone lands at
// the top. Who may is decided before anything else is said
// (accessBusiness.OpenDeleted), so a page someone never had answers as a
// missing one does, live, in the trash or not there at all. A page back under
// a page shows in its body again (FollowRestore), unless placed says the
// editor has put its block back already.
func RestoreDocFromTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	in, ok := readDocRequest(w, r, "restore")
	if !ok {
		return
	}
	if _, err := accessBusiness.OpenDeleted(ctx, who, docRes(in.DocUUID), business.RestoreNeeds); err != nil {
		treeFailed(w, r, "RestoreDocFromTrash", err)
		return
	}
	res, err := business.RestoreDoc(ctx, in.DocUUID, who.UserUID, who.UserUUID, who.ActingAdmin())
	if err != nil {
		treeFailed(w, r, "RestoreDocFromTrash", err)
		return
	}
	if !in.Placed {
		business.FollowRestore(ctx, res, moverOf(who))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Restored.", "data": res})
}

// PurgeDocForGood deletes a doc in the trash for good, with the pages deleted
// with it under it that the person has full access to (POST /doc/purge): for
// its owner, someone with full access, or an admin. The rest stay in the
// trash (kept), moved up to the doc's parent. Who may is decided first, as
// for a restore.
func PurgeDocForGood(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	in, ok := readDocRequest(w, r, "delete for good")
	if !ok {
		return
	}
	id := in.DocUUID
	if _, err := accessBusiness.OpenDeleted(ctx, who, docRes(id), business.PurgeNeeds); err != nil {
		treeFailed(w, r, "PurgeDocForGood", err)
		return
	}
	res, err := business.PurgeDoc(ctx, id, who.UserUID, who.UserUUID, who.ActingAdmin())
	if err != nil {
		treeFailed(w, r, "PurgeDocForGood", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted for good.", "data": res})
}
