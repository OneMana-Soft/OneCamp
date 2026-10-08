// Package monday implements the Import Provider interface for monday.com.
//
// Source: live API only ("api"), GraphQL at https://api.monday.com/v2.
//
// Auth: a personal API token (avatar → Developers → My access tokens),
// sent raw in the Authorization header with a pinned API-Version.
// Stored encrypted via importModels.SaveToken like every other provider.
//
// monday.com data shape:
//
//	Account → Workspace → Board → Group → Item → Subitem
//	Items carry typed column values, files (assets) and updates
//	(threaded comments with replies).
//
// In OneCamp:
//   - One Team per monday workspace that holds an imported board. Boards
//     in the Main workspace (workspace_id null) go under a "Main
//     workspace" team.
//   - One Project per board of type "board". Docs, subitem boards and
//     custom objects are skipped; archived boards only with
//     options.include_archived.
//   - One Task per item; subitems become subtasks. Status comes from the
//     board's status column (the group title when there is none),
//     priority from a column titled "Priority", assignees from a people
//     column, dates from date / timeline columns, labels from tags.
//     Columns not mapped to a field are kept in the description.
//   - One Comment per update and per reply; update files attach to the
//     comment, item files to the task.
//
// Scope: options.workspace_id (from the discover dropdown; "main" for
// the Main workspace) and/or options.board_ids narrow the import.
//
// Rate limits: monday meters complexity points per minute (10M for
// personal tokens, 1M on free/trial), 1,000–5,000 calls per minute by
// plan, and a daily call cap (1,000 on free/basic). The limiter keeps us
// around 10 calls a second; client.go absorbs per-minute resets and
// surfaces the daily cap as ErrRateLimited.
package monday

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"
)

// providerName is the registry key and the import_oauth_tokens.provider
// value (allowed by migration 187).
const providerName = "monday"

// htmlPolicy sanitises update bodies, which are monday-authored HTML.
var htmlPolicy = bluemonday.UGCPolicy()

// Provider implements importProvider.Provider for monday.com.
type Provider struct {
	rl importProvider.Limiter

	mu            sync.Mutex
	snapshotCache map[uuid.UUID]*workspaceSnapshot
}

// New constructs a provider with its own limiter and cache.
func New() *Provider {
	return &Provider{
		// 100ms × burst 5 → ≤600 calls/min, under the lowest plan's
		// 1,000/min with room for a second concurrent import.
		rl:            importProvider.NewSleepLimiter(100*time.Millisecond, 5),
		snapshotCache: make(map[uuid.UUID]*workspaceSnapshot, 4),
	}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }

func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTeams |
		importProvider.CapProjects |
		importProvider.CapTasks |
		importProvider.CapSubtasks |
		importProvider.CapTaskComments |
		importProvider.CapAttachments
}

// CleanupJob evicts the per-job snapshot when the job ends.
func (p *Provider) CleanupJob(jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.snapshotCache {
		if k.String() == jobID {
			delete(p.snapshotCache, k)
		}
	}
}

// DefaultStatusMap covers monday's default status labels, the labels of
// its common templates, and the group titles boards use as workflow.
func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		// Default status column labels.
		"working on it": "inProgress",
		"done":          "done",
		"stuck":         "inProgress",
		"not started":   "todo",
		// Common template labels.
		"in progress":        "inProgress",
		"ready to start":     "todo",
		"to do":              "todo",
		"to-do":              "todo",
		"todo":               "todo",
		"waiting for review": "inReview",
		"in review":          "inReview",
		"review":             "inReview",
		"pending review":     "inReview",
		"on hold":            "backlog",
		"waiting":            "backlog",
		"planning":           "backlog",
		"backlog":            "backlog",
		"future":             "backlog",
		"completed":          "done",
		"complete":           "done",
		"approved":           "done",
		"cancelled":          "canceled",
		"canceled":           "canceled",
		// Group titles of the starter boards.
		"this week":   "todo",
		"next week":   "backlog",
		"new":         "todo",
		"doing":       "inProgress",
		"group title": "todo",
	}
}

// DefaultPriorityMap covers the labels of monday's Priority column
// (emoji stripped: "Critical ⚠️" arrives as "Critical").
func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"critical":    "high",
		"urgent":      "high",
		"high":        "high",
		"medium":      "medium",
		"normal":      "medium",
		"low":         "low",
		"best effort": "low",
	}
}

// ─── Lifecycle ──────────────────────────────────────────────────────

// Validate asks for `me`, the cheapest authenticated query. A bad token
// fails here, at job creation, with a message saying how to fix it.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	return p.validateToken(ctx, tok)
}

func (p *Provider) validateToken(ctx context.Context, tok string) error {
	var resp struct {
		Me *struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"me"`
	}
	if err := p.gql(ctx, tok, `query { me { id name email } }`, nil, &resp); err != nil {
		return fmt.Errorf("monday.com auth: %w", err)
	}
	if resp.Me == nil || resp.Me.ID == "" {
		return errors.New("monday.com auth: the token resolved to no user; paste a personal API token from avatar → Developers → My access tokens")
	}
	return nil
}

// Plan crawls the account once and reports counts, value sets and
// warnings. Idempotent: a re-plan rebuilds the snapshot.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	snap, err := p.buildSnapshot(ctx, j, opts)
	if err != nil {
		return nil, nil, err
	}
	return planFromSnapshot(snap), nil, nil
}

func planFromSnapshot(snap *workspaceSnapshot) *importProvider.Plan {
	plan := &importProvider.Plan{
		UserCount:    len(snap.Users),
		TeamCount:    len(snap.Teams),
		ProjectCount: len(snap.Boards),
	}
	statusSet := map[string]struct{}{}
	prioritySet := map[string]struct{}{}
	for _, t := range snap.Tasks {
		plan.TaskCount++
		if t.ParentTaskID != "" {
			plan.SubtaskCount++
		} else {
			plan.FileCount += len(t.AttachmentRefs)
			for _, a := range t.AttachmentRefs {
				plan.FileBytes += a.Size
			}
		}
		if s := strings.ToLower(strings.TrimSpace(t.Status)); s != "" {
			statusSet[s] = struct{}{}
		}
		if s := strings.ToLower(strings.TrimSpace(t.Priority)); s != "" {
			prioritySet[s] = struct{}{}
		}
	}
	for _, cs := range snap.comments {
		plan.CommentCount += len(cs)
		for _, c := range cs {
			plan.FileCount += len(c.AttachmentRefs)
			for _, a := range c.AttachmentRefs {
				plan.FileBytes += a.Size
			}
		}
	}
	for k := range statusSet {
		plan.StatusValues = append(plan.StatusValues, k)
	}
	for k := range prioritySet {
		plan.PriorityValues = append(plan.PriorityValues, k)
	}
	sort.Strings(plan.StatusValues)
	sort.Strings(plan.PriorityValues)

	if len(snap.Boards) == 0 {
		plan.Warnings = append(plan.Warnings,
			"no boards visible to this token in the chosen scope; the token sees only boards its user can open")
	}
	if n := len(snap.emptyBoards); n > 0 {
		names := snap.emptyBoards
		if len(names) > 5 {
			names = append(append([]string{}, names[:5]...), fmt.Sprintf("and %d more", n-5))
		}
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("%d board(s) have no items and will import as empty projects: %s", n, strings.Join(names, ", ")))
	}
	if snap.skipped > 0 {
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("%d monday doc(s) or non-item board(s) skipped; only item boards import as projects", snap.skipped))
	}
	if plan.SubtaskCount > 0 {
		plan.Warnings = append(plan.Warnings,
			"subitems import as subtasks with their columns; updates and files on subitems are not imported")
	}
	plan.Warnings = append(plan.Warnings, snap.warnings...)
	return plan
}

// IterUsers streams the account's users.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterUsers", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, u := range snap.Users {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    u.ID,
				DisplayName: helpers.FirstNonEmpty(u.Name, u.Email),
				Email:       u.Email,
				AvatarURL:   u.PhotoThumb,
				IsExternal:  u.IsGuest,
				Metadata: map[string]any{
					"monday_id":      u.ID,
					"monday_enabled": u.Enabled,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits one team per monday workspace in scope.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterTeams", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, t := range snap.Teams {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceTeam{
				SourceID:    t.ID,
				Name:        t.Name,
				Description: t.Description,
				MemberIds:   append([]string{}, t.MemberIDs...),
				AdminIds:    append([]string{}, t.AdminIDs...),
				Metadata: map[string]any{
					"monday_workspace_id": t.ID,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterProjects emits one project per board.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterProjects", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, b := range snap.Boards {
			select {
			case <-ctx.Done():
				return
			case out <- boardToSourceProject(b):
			}
		}
	}()
	return out, errCh
}

func boardToSourceProject(b mondayBoard) importProvider.SourceProject {
	members := map[string]bool{}
	for _, u := range b.Subscribers {
		members[u.ID] = true
	}
	admins := make([]string, 0, len(b.Owners))
	for _, u := range b.Owners {
		members[u.ID] = true
		if u.ID != "" {
			admins = append(admins, u.ID)
		}
	}
	creator := ""
	if b.Creator != nil {
		creator = b.Creator.ID
	}
	return importProvider.SourceProject{
		SourceID:     b.ID,
		Name:         helpers.FirstNonEmpty(strings.TrimSpace(b.Name), "Board "+b.ID),
		Description:  helpers.PlainTextToHTML(b.Description),
		TeamSourceID: b.workspaceKey(),
		MemberIds:    sortedKeys(members),
		AdminIds:     admins,
		CreatedBy:    creator,
		Archived:     strings.EqualFold(b.State, "archived"),
		Fields:       fieldsOfBoard(b).list,
		Metadata: map[string]any{
			"monday_url":        b.URL,
			"monday_board_id":   b.ID,
			"monday_board_kind": b.BoardKind,
		},
	}
}

// IterTasksOfProject streams a board's items, then its subitems (with
// ParentTaskID set, which makes the task worker schedule a subtask
// chunk for the parent).
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterTasksOfProject", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, t := range snap.Tasks {
			if t.ProjectSourceID != projectSourceID {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- t:
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask streams one item's subitems from the snapshot.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterSubtasksOfTask", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, idx := range snap.subtasksOf[taskSourceID] {
			select {
			case <-ctx.Done():
				return
			case out <- snap.Tasks[idx]:
			}
		}
	}()
	return out, errCh
}

// IterCommentsOfTask streams an item's updates and replies.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("monday.IterCommentsOfTask", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, c := range snap.comments[taskSourceID] {
			select {
			case <-ctx.Done():
				return
			case out <- c:
			}
		}
	}()
	return out, errCh
}

// FetchAttachment downloads one monday asset. public_url is a presigned
// link that expires within the hour, and the attachment stage can run
// long after Plan, so the URL is re-resolved by asset id first.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", 0, err
	}
	fresh, err := p.resolveAssetURL(ctx, tok, att.SourceID)
	if err != nil {
		return "", 0, err
	}
	if fresh == "" {
		fresh = att.URL
	}
	if fresh == "" {
		return "", 0, importProvider.ErrAttachmentGone
	}
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	dl := att
	dl.URL = fresh
	// Never forward the API token to the storage host.
	dl.Headers = nil
	return importProvider.DefaultFetchAttachment(ctx, dl, dest)
}

func (p *Provider) resolveAssetURL(ctx context.Context, tok, assetID string) (string, error) {
	if assetID == "" {
		return "", nil
	}
	var resp struct {
		Assets []struct {
			ID        string `json:"id"`
			PublicURL string `json:"public_url"`
		} `json:"assets"`
	}
	if err := p.gql(ctx, tok, `query ($ids: [ID!]!) { assets(ids: $ids) { id public_url } }`,
		map[string]any{"ids": []string{assetID}}, &resp); err != nil {
		return "", err
	}
	for _, a := range resp.Assets {
		if a.ID == assetID {
			return a.PublicURL, nil
		}
	}
	return "", nil
}

// ─── Auth ───────────────────────────────────────────────────────────

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j == nil || j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(t.AccessToken) == "" {
		return "", errors.New("no monday.com token saved; connect monday.com first")
	}
	return t.AccessToken, nil
}

// ─── Discoverer ─────────────────────────────────────────────────────

// Discover lists the workspaces the token can see, plus the Main
// workspace, for the "pick a workspace" dropdown. The pick flows back
// as options.workspace_id; no pick imports every workspace.
func (p *Provider) Discover(ctx context.Context, ownerUserID string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("no monday.com token saved")
	}
	tok := token.AccessToken
	out := []importProvider.DiscoverItem{}
	hasMain := false
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		var resp struct {
			Workspaces []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
				Kind        string `json:"kind"`
			} `json:"workspaces"`
		}
		if err := p.gql(ctx, tok, `query ($page: Int!) { workspaces(limit: 100, page: $page) { id name description kind } }`,
			map[string]any{"page": page}, &resp); err != nil {
			return nil, err
		}
		fresh := 0
		for _, w := range resp.Workspaces {
			if w.ID == "" || seen[w.ID] {
				continue
			}
			seen[w.ID] = true
			fresh++
			if strings.EqualFold(strings.TrimSpace(w.Name), mainWorkspaceName) {
				hasMain = true
			}
			out = append(out, importProvider.DiscoverItem{
				ID:          w.ID,
				Name:        w.Name,
				Description: w.Description,
				Kind:        "workspace",
				Meta:        map[string]any{"monday_kind": w.Kind},
			})
		}
		if len(resp.Workspaces) < 100 || fresh == 0 {
			break
		}
	}
	if !hasMain {
		// Unmigrated accounts don't list Main; its boards have no
		// workspace id, so it gets a synthetic one.
		out = append([]importProvider.DiscoverItem{{
			ID:          mainWorkspaceID,
			Name:        mainWorkspaceName,
			Description: "Boards in the account's Main workspace",
			Kind:        "workspace",
		}}, out...)
	}
	return out, nil
}
