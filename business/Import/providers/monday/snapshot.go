package monday

// Snapshot crawler: one coherent pass over the account at Plan time,
// cached per job so every orchestrator stage filters memory instead of
// re-querying monday (whose daily call cap on free/basic plans is 1,000).
//
// Calls per import ≈ users/100 + boards/50 + Σ items/50 (+ one call per
// item with more than 100 updates). Everything an item owns — columns,
// files, updates with replies, subitems — rides on the items query.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

const (
	// maxPages caps every paginated loop so a runaway cursor can't hang
	// a worker. 400 × 50 items = 20k items per board.
	maxPages = 400

	usersPageSize   = 100
	boardsPageSize  = 50
	itemsPageSize   = 50
	updatesPageSize = 100

	// mainWorkspaceID is the synthetic id for monday's Main workspace,
	// whose boards report workspace_id = null on accounts that haven't
	// been migrated to a real Main workspace id.
	mainWorkspaceID   = "main"
	mainWorkspaceName = "Main workspace"
)

// ─── Wire DTOs ──────────────────────────────────────────────────────

type mondayUser struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	PhotoThumb string `json:"photo_thumb"`
	IsGuest    bool   `json:"is_guest"`
	Enabled    bool   `json:"enabled"`
}

type idOnly struct {
	ID string `json:"id"`
}

type mondayWorkspace struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type mondayBoard struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	State       string           `json:"state"`
	BoardKind   string           `json:"board_kind"`
	Type        string           `json:"type"`
	URL         string           `json:"url"`
	WorkspaceID *string          `json:"workspace_id"`
	Workspace   *mondayWorkspace `json:"workspace"`
	Owners      []idOnly         `json:"owners"`
	Subscribers []idOnly         `json:"subscribers"`
	Creator     *idOnly          `json:"creator"`
}

func (b mondayBoard) workspaceKey() string {
	if b.WorkspaceID != nil && *b.WorkspaceID != "" && *b.WorkspaceID != "-1" {
		return *b.WorkspaceID
	}
	if b.Workspace != nil && b.Workspace.ID != "" && b.Workspace.ID != "-1" {
		return b.Workspace.ID
	}
	return mainWorkspaceID
}

// importable: only real item boards. Docs, subitem boards (their items
// arrive nested under the parent item) and custom objects are not tasks.
func (b mondayBoard) importable() bool {
	switch strings.ToLower(b.Type) {
	case "", "board":
		return true
	}
	return false
}

// ─── Snapshot ───────────────────────────────────────────────────────

type teamRec struct {
	ID, Name, Description string
	MemberIDs, AdminIDs   []string
}

type workspaceSnapshot struct {
	Users  []mondayUser
	Teams  []teamRec
	Boards []mondayBoard
	// Tasks holds items and subitems, already mapped. Subitems carry
	// ParentTaskID and are listed after their parent.
	Tasks []importProvider.SourceTask

	comments    map[string][]importProvider.SourceComment
	subtasksOf  map[string][]int
	emptyBoards []string
	skipped     int
	warnings    []string
}

// scope is the operator's narrowing, from job options.
type scope struct {
	WorkspaceID     string
	BoardIDs        map[string]bool
	IncludeArchived bool
}

func scopeFrom(j *importModels.Job, opts importProvider.JobOptions) scope {
	m := map[string]any{}
	if j != nil && len(j.Options) > 0 {
		_ = json.Unmarshal(j.Options, &m)
	}
	for k, v := range opts {
		m[k] = v
	}
	s := scope{BoardIDs: map[string]bool{}}
	if v, ok := m["workspace_id"].(string); ok {
		s.WorkspaceID = strings.TrimSpace(v)
	}
	switch v := m["board_ids"].(type) {
	case []any:
		for _, x := range v {
			if id := strings.TrimSpace(fmt.Sprint(x)); id != "" {
				s.BoardIDs[id] = true
			}
		}
	case []string:
		for _, id := range v {
			if id = strings.TrimSpace(id); id != "" {
				s.BoardIDs[id] = true
			}
		}
	case string:
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				s.BoardIDs[id] = true
			}
		}
	}
	if v, ok := m["include_archived"].(bool); ok {
		s.IncludeArchived = v
	}
	return s
}

func (p *Provider) buildSnapshot(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*workspaceSnapshot, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}
	snap, err := p.crawl(ctx, tok, scopeFrom(j, opts))
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.snapshotCache[j.Id] = snap
	p.mu.Unlock()
	return snap, nil
}

func (p *Provider) snapshotFor(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*workspaceSnapshot, error) {
	p.mu.Lock()
	if s, ok := p.snapshotCache[j.Id]; ok {
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	return p.buildSnapshot(ctx, j, opts)
}

// crawl is the token-level walk, separated from the job plumbing so the
// transport tests can drive it end to end against a fake server.
func (p *Provider) crawl(ctx context.Context, tok string, sc scope) (*workspaceSnapshot, error) {
	users, err := p.fetchUsers(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("monday.com users: %w", err)
	}
	boards, skipped, err := p.fetchBoards(ctx, tok, sc)
	if err != nil {
		return nil, fmt.Errorf("monday.com boards: %w", err)
	}

	snap := &workspaceSnapshot{
		Users:      users,
		Boards:     boards,
		comments:   map[string][]importProvider.SourceComment{},
		subtasksOf: map[string][]int{},
		skipped:    skipped,
	}

	for _, b := range boards {
		items, err := p.fetchBoardItems(ctx, tok, b.ID)
		if err != nil {
			return nil, fmt.Errorf("monday.com items for board %q: %w", b.Name, err)
		}
		if len(items) == 0 {
			snap.emptyBoards = append(snap.emptyBoards, b.Name)
		}
		for _, it := range items {
			if len(it.Updates) >= updatesPageSize {
				more, err := p.fetchMoreUpdates(ctx, tok, it.ID)
				if err != nil {
					return nil, fmt.Errorf("monday.com updates for item %s: %w", it.ID, err)
				}
				it.Updates = append(it.Updates, more...)
			}
			comments := updatesToComments(it.ID, it.Updates)
			commentAssets := map[string]bool{}
			for _, u := range it.Updates {
				for _, a := range u.Assets {
					commentAssets[a.ID] = true
				}
			}
			snap.comments[it.ID] = comments
			snap.Tasks = append(snap.Tasks, p.itemToSourceTask(it, b.ID, "", len(comments), commentAssets))
			for _, sub := range it.Subitems {
				snap.subtasksOf[it.ID] = append(snap.subtasksOf[it.ID], len(snap.Tasks))
				snap.Tasks = append(snap.Tasks, p.itemToSourceTask(sub, b.ID, it.ID, 0, nil))
			}
		}
	}

	snap.Teams = p.buildTeams(ctx, tok, boards, snap)
	return snap, nil
}

// buildTeams emits one team per monday workspace that holds an
// imported board. Members are the union of the workspace's subscribers
// (best effort — see fetchWorkspaceMembers) and its boards' subscribers.
func (p *Provider) buildTeams(ctx context.Context, tok string, boards []mondayBoard, snap *workspaceSnapshot) []teamRec {
	order := []string{}
	byID := map[string]*teamRec{}
	members := map[string]map[string]bool{}
	realIDs := []string{}
	for _, b := range boards {
		key := b.workspaceKey()
		if _, ok := byID[key]; !ok {
			t := &teamRec{ID: key, Name: mainWorkspaceName}
			if key != mainWorkspaceID {
				realIDs = append(realIDs, key)
				if b.Workspace != nil {
					t.Name = b.Workspace.Name
					t.Description = b.Workspace.Description
				}
				if t.Name == "" {
					t.Name = "Workspace " + key
				}
			}
			byID[key] = t
			members[key] = map[string]bool{}
			order = append(order, key)
		}
		for _, u := range b.Subscribers {
			members[key][u.ID] = true
		}
		for _, u := range b.Owners {
			members[key][u.ID] = true
		}
	}

	if len(realIDs) > 0 {
		ws, err := p.fetchWorkspaceMembers(ctx, tok, realIDs)
		if err != nil {
			snap.warnings = append(snap.warnings,
				"could not read workspace members ("+err.Error()+"); teams get their boards' subscribers only")
		}
		for _, w := range ws {
			t := byID[w.ID]
			if t == nil {
				continue
			}
			for _, u := range w.Users {
				members[w.ID][u.ID] = true
			}
			for _, u := range w.Owners {
				members[w.ID][u.ID] = true
				t.AdminIDs = append(t.AdminIDs, u.ID)
			}
		}
	}

	out := make([]teamRec, 0, len(order))
	for _, key := range order {
		t := byID[key]
		t.MemberIDs = sortedKeys(members[key])
		out = append(out, *t)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ─── Fetchers ───────────────────────────────────────────────────────

const usersQuery = `query ($page: Int!, $limit: Int!) {
  users(limit: $limit, page: $page) { id name email photo_thumb is_guest enabled }
}`

func (p *Provider) fetchUsers(ctx context.Context, tok string) ([]mondayUser, error) {
	out := []mondayUser{}
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		var resp struct {
			Users []mondayUser `json:"users"`
		}
		if err := p.gql(ctx, tok, usersQuery, map[string]any{"page": page, "limit": usersPageSize}, &resp); err != nil {
			return nil, err
		}
		fresh := 0
		for _, u := range resp.Users {
			if u.ID == "" || seen[u.ID] {
				continue
			}
			seen[u.ID] = true
			fresh++
			out = append(out, u)
		}
		// A short page ends it; so does a page with nothing new (a
		// server that ignores `page` would otherwise loop to the cap).
		if len(resp.Users) < usersPageSize || fresh == 0 {
			break
		}
	}
	return out, nil
}

const boardFields = `id name description state board_kind type url workspace_id
    workspace { id name description }
    owners { id } subscribers { id } creator { id }`

func boardsQuery(withWorkspace bool, includeArchived bool) string {
	state := "active"
	if includeArchived {
		state = "all"
	}
	if withWorkspace {
		return `query ($page: Int!, $limit: Int!, $ws: [ID]) {
  boards(limit: $limit, page: $page, state: ` + state + `, order_by: created_at, workspace_ids: $ws) { ` + boardFields + ` }
}`
	}
	return `query ($page: Int!, $limit: Int!) {
  boards(limit: $limit, page: $page, state: ` + state + `, order_by: created_at) { ` + boardFields + ` }
}`
}

// fetchBoards returns the importable boards in scope and how many
// visible boards were skipped as not task-shaped (docs etc.).
func (p *Provider) fetchBoards(ctx context.Context, tok string, sc scope) ([]mondayBoard, int, error) {
	// The Main workspace can't be filtered by id (it has none on most
	// accounts), so it's fetched unfiltered and narrowed below.
	filterWS := sc.WorkspaceID != "" && sc.WorkspaceID != mainWorkspaceID
	q := boardsQuery(filterWS, sc.IncludeArchived)

	out := []mondayBoard{}
	seen := map[string]bool{}
	skipped := 0
	for page := 1; page <= maxPages; page++ {
		vars := map[string]any{"page": page, "limit": boardsPageSize}
		if filterWS {
			vars["ws"] = []string{sc.WorkspaceID}
		}
		var resp struct {
			Boards []mondayBoard `json:"boards"`
		}
		if err := p.gql(ctx, tok, q, vars, &resp); err != nil {
			return nil, 0, err
		}
		fresh := 0
		for _, b := range resp.Boards {
			if b.ID == "" || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			fresh++
			if sc.WorkspaceID == mainWorkspaceID && b.workspaceKey() != mainWorkspaceID {
				continue
			}
			if len(sc.BoardIDs) > 0 && !sc.BoardIDs[b.ID] {
				continue
			}
			if !b.importable() {
				skipped++
				continue
			}
			if strings.EqualFold(b.State, "deleted") {
				continue
			}
			if !sc.IncludeArchived && strings.EqualFold(b.State, "archived") {
				continue
			}
			out = append(out, b)
		}
		if len(resp.Boards) < boardsPageSize || fresh == 0 {
			break
		}
	}
	return out, skipped, nil
}

// itemCore is shared by items and subitems. Subitems skip updates and
// files: the orchestrator's subtask stage imports neither.
const itemCore = `id name state url created_at updated_at creator_id
      group { id title }
      column_values { id type text value column { title } }`

const assetFields = `id name public_url file_extension file_size`

const updateFields = `id body text_body created_at creator_id
        assets { ` + assetFields + ` }
        replies { id body created_at creator_id }`

var itemFields = itemCore + `
      assets { ` + assetFields + ` }
      updates(limit: ` + fmt.Sprint(updatesPageSize) + `) { ` + updateFields + ` }
      subitems { ` + itemCore + ` }`

var firstItemsQuery = `query ($ids: [ID!], $limit: Int!) {
  boards(ids: $ids) {
    items_page(limit: $limit) {
      cursor
      items { ` + itemFields + ` }
    }
  }
}`

var nextItemsQuery = `query ($cursor: String!, $limit: Int!) {
  next_items_page(cursor: $cursor, limit: $limit) {
    cursor
    items { ` + itemFields + ` }
  }
}`

type itemsPage struct {
	Cursor *string      `json:"cursor"`
	Items  []mondayItem `json:"items"`
}

// fetchBoardItems walks items_page then next_items_page until the
// cursor runs out. monday cursors outlive a board's crawl by a wide
// margin; a repeated cursor or an empty page also ends the walk.
func (p *Provider) fetchBoardItems(ctx context.Context, tok, boardID string) ([]mondayItem, error) {
	var first struct {
		Boards []struct {
			ItemsPage itemsPage `json:"items_page"`
		} `json:"boards"`
	}
	if err := p.gql(ctx, tok, firstItemsQuery, map[string]any{"ids": []string{boardID}, "limit": itemsPageSize}, &first); err != nil {
		return nil, err
	}
	if len(first.Boards) == 0 {
		return nil, nil
	}
	out := append([]mondayItem{}, first.Boards[0].ItemsPage.Items...)
	cursor := deref(first.Boards[0].ItemsPage.Cursor)
	for page := 0; cursor != "" && page < maxPages; page++ {
		var next struct {
			Next itemsPage `json:"next_items_page"`
		}
		if err := p.gql(ctx, tok, nextItemsQuery, map[string]any{"cursor": cursor, "limit": itemsPageSize}, &next); err != nil {
			return nil, err
		}
		out = append(out, next.Next.Items...)
		nc := deref(next.Next.Cursor)
		if nc == cursor || len(next.Next.Items) == 0 {
			break
		}
		cursor = nc
	}
	return out, nil
}

var moreUpdatesQuery = `query ($ids: [ID!], $page: Int!) {
  items(ids: $ids) {
    id
    updates(limit: ` + fmt.Sprint(updatesPageSize) + `, page: $page) { ` + updateFields + ` }
  }
}`

// fetchMoreUpdates pages an item's updates past the first page the
// items query embedded.
func (p *Provider) fetchMoreUpdates(ctx context.Context, tok, itemID string) ([]mondayUpdate, error) {
	out := []mondayUpdate{}
	for page := 2; page <= maxPages; page++ {
		var resp struct {
			Items []struct {
				Updates []mondayUpdate `json:"updates"`
			} `json:"items"`
		}
		if err := p.gql(ctx, tok, moreUpdatesQuery, map[string]any{"ids": []string{itemID}, "page": page}, &resp); err != nil {
			return nil, err
		}
		if len(resp.Items) == 0 {
			break
		}
		out = append(out, resp.Items[0].Updates...)
		if len(resp.Items[0].Updates) < updatesPageSize {
			break
		}
	}
	return out, nil
}

type workspaceMembers struct {
	ID     string   `json:"id"`
	Users  []idOnly `json:"users_subscribers"`
	Owners []idOnly `json:"owners_subscribers"`
}

const workspaceMembersQuery = `query ($ids: [ID]) {
  workspaces(ids: $ids) { id users_subscribers { id } owners_subscribers { id } }
}`

// fetchWorkspaceMembers is best effort: subscriber lists default to 25
// entries and need workspace visibility the token may not have, so a
// failure here degrades team membership rather than failing the import.
func (p *Provider) fetchWorkspaceMembers(ctx context.Context, tok string, ids []string) ([]workspaceMembers, error) {
	var resp struct {
		Workspaces []workspaceMembers `json:"workspaces"`
	}
	if err := p.gql(ctx, tok, workspaceMembersQuery, map[string]any{"ids": ids}, &resp); err != nil {
		return nil, err
	}
	return resp.Workspaces, nil
}
