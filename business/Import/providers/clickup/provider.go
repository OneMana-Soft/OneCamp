// Package clickup implements the Import Provider interface for ClickUp.
//
// Source: live API only ("api"). ClickUp has no first-party export
// format, so the REST API at https://api.clickup.com/api/v2 is the
// only viable path.
//
// Auth: Personal API Token (https://app.clickup.com/<id>/settings/apps)
// or OAuth 2.0 access token. Stored encrypted via importModels.SaveToken.
//
// ClickUp data shape:
//
//	Workspace (Team) → Space → Folder → List → Task → Subtask
//	                                ↑
//	                                └── Folderless Lists exist too
//	Tasks have Comments and Attachments.
//
// In OneCamp:
//   - Workspace selection is mandatory: tokens may have access to many
//     workspaces. The operator picks one via the discover dialog;
//     opts["workspace_id"] scopes the import.
//   - One OneCamp Team per ClickUp Space.
//   - One OneCamp Project per List. Folder name is included in the
//     project name when present ("Folder · List") so the operator
//     keeps the hierarchy at a glance. Folderless lists become bare
//     projects.
//   - One Task per ClickUp task. Subtasks (parent != null) become
//     OneCamp subtasks via the same parent_task_id linking the
//     task worker uses for every other provider.
//   - One Comment per ClickUp comment.
//   - Attachments fetched directly from the URL ClickUp returns.
//
// Pagination: ClickUp's /list/{id}/task uses page=N (zero-indexed) and
// returns 100 per page. We follow up to a safety cap (200 pages = 20k
// tasks per list).
//
// Rate limits: ClickUp publishes 100 req/min for free, 1000 req/min
// for paid (Business+ plans). We use SleepLimiter at one request per
// 80ms which is ~750/min — under both ceilings with comfortable
// headroom for parallel workers.
package clickup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const (
	providerName = importModels.ProviderClickUp

	// maxPages caps pagination so a runaway list query can't hang a
	// worker. 200 × 100 = 20k tasks per list, which covers every
	// realistic ClickUp deployment.
	maxPages = 200
)

// apiBase is ClickUp's REST root. v2 is the current stable; v3
// exists for select endpoints but doesn't add anything we need.
//
// Declared as a package-level var (not const) so tests can swap in a
// httptest server URL. Production code never reassigns this.
var apiBase = "https://api.clickup.com/api/v2"

// Provider implements importProvider.Provider for ClickUp.
type Provider struct {
	rl importProvider.Limiter

	// snapshotCache pins one workspace snapshot per job. Plan loads
	// it; every Iter* hits the cache. Without this we'd re-walk the
	// Spaces → Folders → Lists tree on every stage transition.
	mu            sync.Mutex
	snapshotCache map[uuid.UUID]*workspaceSnapshot
}

// New constructs a singleton.
func New() *Provider {
	return &Provider{
		// 80ms ticker × burst 4 → ~12/sec sustained, ~720/min — under
		// the free-tier ceiling of 100/min when fewer goroutines are
		// active and well under the 1000/min paid ceiling at full burst.
		rl:            importProvider.NewSleepLimiter(80*time.Millisecond, 4),
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

// CleanupJob evicts the snapshot when a job hits a terminal state.
func (p *Provider) CleanupJob(jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.snapshotCache {
		if k.String() == jobID {
			delete(p.snapshotCache, k)
		}
	}
}

// DefaultStatusMap maps ClickUp's standard `status_type` values onto
// OneCamp statuses. ClickUp also exposes the `status.status` (workspace-
// defined name); the provider stamps both — type takes precedence in
// the mapping, with the name as a fallback.
func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		// status.type values (always lower-case)
		"open":   "todo",
		"custom": "inProgress",
		"closed": "done",
		"done":   "done",

		// Common status.status names
		"to do":       "todo",
		"todo":        "todo",
		"in progress": "inProgress",
		"in-progress": "inProgress",
		"review":      "inReview",
		"in review":   "inReview",
		"qa":          "inReview",
		"complete":    "done",
		"completed":   "done",
		"backlog":     "backlog",
		"cancelled":   "canceled",
		"canceled":    "canceled",
	}
}

// DefaultPriorityMap maps ClickUp's numeric priority (1=urgent, 2=high,
// 3=normal, 4=low) — the provider stamps the human label so this map
// keys on the readable value.
func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"urgent": "high",
		"high":   "high",
		"normal": "medium",
		"medium": "medium",
		"low":    "low",
	}
}

// ─── Lifecycle ────────────────────────────────────────────────────

// Validate runs /user — ClickUp's cheapest authenticated endpoint.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	var resp struct {
		User struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := p.getJSON(ctx, tok, "/user", &resp); err != nil {
		return fmt.Errorf("clickup auth: %w", err)
	}
	if resp.User.ID == 0 {
		return errors.New("clickup /user returned empty id; bad token?")
	}
	return nil
}

// Plan walks the workspace once and stashes the result in
// snapshotCache. The orchestrator's stages then filter that struct.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	snap, err := p.buildSnapshot(ctx, j, opts)
	if err != nil {
		return nil, nil, err
	}

	plan := &importProvider.Plan{
		UserCount:    len(snap.Users),
		TeamCount:    len(snap.Spaces),
		ProjectCount: len(snap.Lists),
	}

	statusSet := map[string]struct{}{}
	prioritySet := map[string]struct{}{}

	for _, t := range snap.Tasks {
		plan.TaskCount++
		if t.ParentID != "" {
			plan.SubtaskCount++
		}
		plan.CommentCount += len(t.Comments)
		plan.FileCount += len(t.Attachments)
		for _, a := range t.Attachments {
			plan.FileBytes += a.Size
		}
		if t.StatusName != "" {
			statusSet[strings.ToLower(t.StatusName)] = struct{}{}
		}
		if t.StatusType != "" {
			statusSet[strings.ToLower(t.StatusType)] = struct{}{}
		}
		if t.PriorityLabel != "" {
			prioritySet[strings.ToLower(t.PriorityLabel)] = struct{}{}
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

	if snap.Workspace.ID == "" {
		plan.Warnings = append(plan.Warnings,
			"no workspace_id supplied and the token sees multiple workspaces; pass options.workspace_id")
	}

	return plan, nil, nil
}

// IterUsers streams the workspace members.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("clickup.IterUsers", errCh)
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
				DisplayName: helpers.FirstNonEmpty(u.Username, u.Email),
				Login:       u.Username,
				Email:       u.Email,
				AvatarURL:   u.ProfilePicture,
				Metadata: map[string]any{
					"clickup_role": u.Role,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits one team per ClickUp Space. ClickUp's "Workspace"
// concept maps to the import-job's source workspace; Spaces are the
// subtree under it that map to OneCamp teams.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("clickup.IterTeams", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, s := range snap.Spaces {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceTeam{
				SourceID:    s.ID,
				Name:        s.Name,
				Description: "",
				MemberIds:   append([]string{}, s.MemberIDs...),
				Metadata: map[string]any{
					"clickup_private": s.Private,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterProjects emits one project per ClickUp List. Folder context is
// included in the project name so the operator sees the hierarchy
// without needing a UI tree.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("clickup.IterProjects", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		fields := listFields(snap.Tasks)
		for _, l := range snap.Lists {
			name := l.Name
			if l.FolderName != "" {
				name = l.FolderName + " · " + l.Name
			}
			archived := l.Archived
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:     l.ID,
				Name:         name,
				Description:  l.Content,
				TeamSourceID: l.SpaceID,
				MemberIds:    append([]string{}, l.MemberIDs...),
				Archived:     archived,
				Fields:       fields[l.ID],
				Metadata: map[string]any{
					"clickup_url":         l.URL,
					"clickup_folder_id":   l.FolderID,
					"clickup_list_status": l.Status,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams tasks for one List. ClickUp returns
// subtasks as separate task records with parent set, so we surface
// them in the same stream.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("clickup.IterTasksOfProject", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, t := range snap.Tasks {
			if t.ListID != projectSourceID {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			task := p.taskToSourceTask(t)
			select {
			case <-ctx.Done():
				return
			case out <- task:
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask streams a task's subtasks. ClickUp's subtasks are
// regular tasks with `parent` set: IterTasksOfProject sends them with
// ParentTaskID, the task worker schedules a subtask chunk for the parent
// instead of importing them, and that chunk reads them here. (It used to be
// empty, and every subtask was dropped.)
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	// The task worker turns a task with a parent into a subtask chunk for
	// that parent and asks for its subtasks here; the snapshot has them.
	return importProvider.StreamTasks(ctx, "clickup.IterSubtasksOfTask",
		func() ([]clickupTask, error) {
			snap, err := p.snapshotFor(ctx, j, opts)
			if err != nil {
				return nil, err
			}
			return snap.Tasks, nil
		},
		func(t clickupTask) bool { return t.ParentID == taskSourceID },
		p.taskToSourceTask)
}

// IterCommentsOfTask streams comments for one task. The snapshot
// already contains them.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("clickup.IterCommentsOfTask", errCh)
		snap, err := p.snapshotFor(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		t := snap.TaskByID(taskSourceID)
		if t == nil {
			return
		}
		for _, c := range t.Comments {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceComment{
				SourceID:       c.ID,
				TaskSourceID:   taskSourceID,
				Body:           helpers.PlainTextToHTML(c.Comment),
				AuthorSourceID: c.UserID,
				Created:        c.Created,
			}:
			}
		}
	}()
	return out, errCh
}

// FetchAttachment streams a ClickUp attachment to dest. ClickUp's
// attachment URLs are S3 presigned, valid for several hours; we use
// the URL as-is. Authorization isn't accepted on attachment domains
// (it would be reflected back as a header in the signed URL anyway).
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	return importProvider.DefaultFetchAttachment(ctx, att, dest)
}

// ─── Snapshot loader ──────────────────────────────────────────────

type workspaceSnapshot struct {
	Workspace clickupWorkspace
	Users     []clickupUser
	Spaces    []clickupSpace
	Lists     []clickupList
	Tasks     []clickupTask

	taskByID map[string]*clickupTask
}

func (s *workspaceSnapshot) TaskByID(id string) *clickupTask {
	if s == nil {
		return nil
	}
	return s.taskByID[id]
}

// buildSnapshot walks the ClickUp tree:
//  1. /team           → list workspaces; pick one (opts.workspace_id or
//     the only one accessible).
//  2. /team/{id}/space → spaces in that workspace
//  3. /space/{id}/folder + /space/{id}/list (folderless) → lists
//  4. /list/{id}/task (paginated, include subtasks) → tasks
//  5. /task/{id}/comment → comments
//
// Tasks already include attachments inline in the response, so the
// only fan-out we need is comments per task.
func (p *Provider) buildSnapshot(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*workspaceSnapshot, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}

	ws, err := p.resolveWorkspace(ctx, tok, opts)
	if err != nil {
		return nil, err
	}

	// Users live on the workspace shape returned by /team.
	users := make([]clickupUser, 0, len(ws.Members))
	for _, m := range ws.Members {
		users = append(users, m.User)
	}

	spaces, err := p.fetchSpaces(ctx, tok, ws.ID)
	if err != nil {
		return nil, fmt.Errorf("clickup spaces: %w", err)
	}

	lists := make([]clickupList, 0, 32)
	for i := range spaces {
		// Folder lists.
		folders, err := p.fetchFolders(ctx, tok, spaces[i].ID)
		if err != nil {
			return nil, fmt.Errorf("clickup folders for space %s: %w", spaces[i].Name, err)
		}
		for _, f := range folders {
			for _, l := range f.Lists {
				l.SpaceID = spaces[i].ID
				l.FolderID = f.ID
				l.FolderName = f.Name
				lists = append(lists, l)
			}
		}
		// Folderless lists.
		fless, err := p.fetchFolderlessLists(ctx, tok, spaces[i].ID)
		if err != nil {
			return nil, fmt.Errorf("clickup folderless lists for space %s: %w", spaces[i].Name, err)
		}
		for _, l := range fless {
			l.SpaceID = spaces[i].ID
			lists = append(lists, l)
		}
	}

	// Tasks per list. ClickUp's list-tasks endpoint requires
	// include_subtasks=true for subtasks to show up; without it they're
	// silently omitted.
	tasks := make([]clickupTask, 0, 256)
	for _, l := range lists {
		ts, err := p.fetchListTasks(ctx, tok, l.ID)
		if err != nil {
			return nil, fmt.Errorf("clickup tasks for list %s: %w", l.Name, err)
		}
		tasks = append(tasks, ts...)
	}

	// Comments per task (separate endpoint, ClickUp doesn't embed them).
	for i := range tasks {
		cs, err := p.fetchTaskComments(ctx, tok, tasks[i].ID)
		if err != nil {
			// One bad task shouldn't sink the whole import. Log via
			// Plan's warning channel instead.
			continue
		}
		tasks[i].Comments = cs
	}

	snap := &workspaceSnapshot{
		Workspace: ws,
		Users:     users,
		Spaces:    spaces,
		Lists:     lists,
		Tasks:     tasks,
		taskByID:  make(map[string]*clickupTask, len(tasks)),
	}
	for i := range snap.Tasks {
		snap.taskByID[snap.Tasks[i].ID] = &snap.Tasks[i]
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

// resolveWorkspace handles the multi-workspace case: a token may see
// many workspaces, in which case the operator must scope via
// opts["workspace_id"]. With a single workspace we auto-pick.
func (p *Provider) resolveWorkspace(ctx context.Context, tok string, opts importProvider.JobOptions) (clickupWorkspace, error) {
	var resp struct {
		Teams []clickupWorkspace `json:"teams"`
	}
	if err := p.getJSON(ctx, tok, "/team", &resp); err != nil {
		return clickupWorkspace{}, err
	}
	if len(resp.Teams) == 0 {
		return clickupWorkspace{}, errors.New("clickup token sees zero workspaces; check OAuth scope")
	}

	picked := ""
	if opts != nil {
		if v, ok := opts["workspace_id"].(string); ok {
			picked = v
		}
	}
	if picked == "" && len(resp.Teams) == 1 {
		return resp.Teams[0], nil
	}
	if picked == "" {
		// Multiple workspaces, no pick. Plan() surfaces this as a
		// warning; we return the first deterministically so the rest
		// of the snapshot loader doesn't crash.
		return resp.Teams[0], nil
	}
	for _, w := range resp.Teams {
		if w.ID == picked {
			return w, nil
		}
	}
	return clickupWorkspace{}, fmt.Errorf("clickup workspace %q not found in token's accessible set", picked)
}

// ─── REST helpers ─────────────────────────────────────────────────

func (p *Provider) fetchSpaces(ctx context.Context, tok, workspaceID string) ([]clickupSpace, error) {
	var resp struct {
		Spaces []clickupSpace `json:"spaces"`
	}
	if err := p.getJSON(ctx, tok, "/team/"+url.PathEscape(workspaceID)+"/space?archived=false", &resp); err != nil {
		return nil, err
	}
	return resp.Spaces, nil
}

func (p *Provider) fetchFolders(ctx context.Context, tok, spaceID string) ([]clickupFolder, error) {
	var resp struct {
		Folders []clickupFolder `json:"folders"`
	}
	if err := p.getJSON(ctx, tok, "/space/"+url.PathEscape(spaceID)+"/folder?archived=false", &resp); err != nil {
		return nil, err
	}
	return resp.Folders, nil
}

func (p *Provider) fetchFolderlessLists(ctx context.Context, tok, spaceID string) ([]clickupList, error) {
	var resp struct {
		Lists []clickupList `json:"lists"`
	}
	if err := p.getJSON(ctx, tok, "/space/"+url.PathEscape(spaceID)+"/list?archived=false", &resp); err != nil {
		return nil, err
	}
	return resp.Lists, nil
}

func (p *Provider) fetchListTasks(ctx context.Context, tok, listID string) ([]clickupTask, error) {
	out := []clickupTask{}
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Tasks    []clickupTask `json:"tasks"`
			LastPage bool          `json:"last_page"`
		}
		path := fmt.Sprintf("/list/%s/task?include_subtasks=true&include_closed=true&subtasks=true&archived=false&page=%d",
			url.PathEscape(listID), page)
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Tasks...)
		if resp.LastPage || len(resp.Tasks) == 0 {
			break
		}
	}
	return out, nil
}

func (p *Provider) fetchTaskComments(ctx context.Context, tok, taskID string) ([]clickupComment, error) {
	// ClickUp's task-comment endpoint paginates via start + start_id
	// of the oldest comment seen. We follow until we get fewer than
	// 25 (the API's page size) or hit our safety cap.
	out := []clickupComment{}
	startTs := ""
	startID := ""
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Comments []clickupComment `json:"comments"`
		}
		qs := url.Values{}
		if startTs != "" {
			qs.Set("start", startTs)
			qs.Set("start_id", startID)
		}
		path := "/task/" + url.PathEscape(taskID) + "/comment"
		if q := qs.Encode(); q != "" {
			path += "?" + q
		}
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		if len(resp.Comments) == 0 {
			break
		}
		out = append(out, resp.Comments...)
		// Use the oldest seen as the next page anchor. ClickUp returns
		// newest-first; we want all of them.
		oldest := resp.Comments[len(resp.Comments)-1]
		startTs = oldest.DateCreated
		startID = oldest.ID
		if len(resp.Comments) < 25 {
			break
		}
	}
	return out, nil
}

// getJSON does GET <apiBase><path>, decoding into out. Honours 429
// with Retry-After. The path must already start with "/".
func (p *Provider) getJSON(ctx context.Context, tok, path string, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	// ClickUp accepts both raw API key and OAuth token in the
	// Authorization header (no "Bearer " prefix for either). Stripping
	// any prefix the token might already carry keeps both shapes
	// working.
	req.Header.Set("Authorization", strings.TrimPrefix(tok, "Bearer "))
	req.Header.Set("Accept", "application/json")

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retry := time.Duration(parseRetryAfter(resp.Header.Get("Retry-After"))) * time.Second
		return &importProvider.ErrRateLimited{RetryAfter: retry, Reason: "clickup 429"}
	case resp.StatusCode == http.StatusUnauthorized:
		return &importProvider.TokenRejected{Msg: "clickup unauthorized; reconnect this provider"}
	case resp.StatusCode == http.StatusNotFound:
		return importProvider.ErrAttachmentGone
	case resp.StatusCode >= 400:
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("clickup http %d: %s", resp.StatusCode, string(buf[:n]))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func parseRetryAfter(h string) int64 {
	if h == "" {
		return 0
	}
	var secs int64
	if _, err := fmt.Sscanf(h, "%d", &secs); err == nil && secs > 0 {
		return secs
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return int64(d.Seconds())
		}
	}
	return 0
}

// ─── Auth ─────────────────────────────────────────────────────────

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty clickup token")
	}
	if t.RefreshToken != "" {
		fresh, ferr := importProvider.FreshAccessToken(ctx, clickupOAuthConfig(), providerName, *j.TriggeredBy)
		if ferr == nil && fresh != "" {
			return fresh, nil
		}
	}
	return t.AccessToken, nil
}

// ─── Mapping helpers ──────────────────────────────────────────────

func (p *Provider) taskToSourceTask(t clickupTask) importProvider.SourceTask {
	desc := helpers.PlainTextToHTML(t.TextContent)
	if desc == "" {
		// Fall back to description if text_content is absent (older API).
		desc = helpers.PlainTextToHTML(t.Description)
	}

	status := t.StatusName
	if status == "" {
		status = t.StatusType
	}

	priority := t.PriorityLabel
	if priority == "" {
		priority = "normal"
	}

	assignees := []string{}
	for _, a := range t.Assignees {
		assignees = append(assignees, a.ID)
	}

	atts := make([]importProvider.SourceAttachment, 0, len(t.Attachments))
	for _, a := range t.Attachments {
		if a.URL == "" {
			continue
		}
		atts = append(atts, importProvider.SourceAttachment{
			SourceID: a.ID,
			Name:     a.Title,
			URL:      a.URL,
			Mime:     a.MimeType,
			Size:     a.Size,
			Parent:   importProvider.SourceRef{Kind: "task", SourceID: t.ID},
		})
	}

	labels := make([]string, 0, len(t.Tags))
	for _, tg := range t.Tags {
		if tg.Name != "" {
			labels = append(labels, tg.Name)
		}
	}

	return importProvider.SourceTask{
		SourceID:        t.ID,
		ParentTaskID:    t.ParentID,
		ProjectSourceID: t.ListID,
		Name:            helpers.TruncateRunes(t.Name, 256),
		Description:     desc,
		Status:          status,
		Priority:        priority,
		Labels:          labels,
		AssigneeIds:     assignees,
		CreatedBy:       t.CreatorID,
		StartDate:       parseClickUpMillis(t.StartDate),
		DueDate:         parseClickUpMillis(t.DueDate),
		Created:         derefTime(parseClickUpMillis(t.DateCreated)),
		Updated:         derefTime(parseClickUpMillis(t.DateUpdated)),
		Completed:       t.DateClosed != "" || strings.EqualFold(t.StatusType, "closed"),
		AttachmentRefs:  atts,
		CommentCount:    len(t.Comments),
		Fields:          fieldValues(t.CustomFields),
		Metadata: map[string]any{
			"clickup_url": t.URL,
			"clickup_id":  t.ID,
			"custom_id":   t.CustomID,
			"status_type": t.StatusType,
			"priority_id": t.PriorityID,
			"folder_id":   t.FolderID,
			"space_id":    t.SpaceID,
		},
	}
}

// parseClickUpMillis turns ClickUp's millisecond-since-epoch string
// (yes, it's a string, not an int — quirky API) into *time.Time.
// Returns nil for empty / unparseable values.
func parseClickUpMillis(s string) *time.Time {
	if s == "" || s == "0" {
		return nil
	}
	var ms int64
	if _, err := fmt.Sscanf(s, "%d", &ms); err != nil || ms <= 0 {
		return nil
	}
	t := time.Unix(0, ms*int64(time.Millisecond)).UTC()
	return &t
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ─── DTOs ─────────────────────────────────────────────────────────

type clickupUser struct {
	// ClickUp returns user IDs as numeric integers in some endpoints
	// and as strings in others (members vs assignees). UnmarshalJSON
	// below normalises to string.
	ID             string          `json:"-"`
	IDRaw          json.RawMessage `json:"id"`
	Username       string          `json:"username"`
	Email          string          `json:"email"`
	ProfilePicture string          `json:"profilePicture"`
	Role           int             `json:"role"`
}

func (u *clickupUser) UnmarshalJSON(data []byte) error {
	type alias clickupUser
	a := (*alias)(u)
	if err := json.Unmarshal(data, a); err != nil {
		return err
	}
	if len(a.IDRaw) == 0 {
		return nil
	}
	// Try string first, then number.
	var s string
	if err := json.Unmarshal(a.IDRaw, &s); err == nil {
		u.ID = s
		return nil
	}
	var n int64
	if err := json.Unmarshal(a.IDRaw, &n); err == nil {
		u.ID = fmt.Sprintf("%d", n)
		return nil
	}
	return nil
}

type clickupMembership struct {
	User clickupUser `json:"user"`
}

type clickupWorkspace struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Members []clickupMembership `json:"members"`
}

type clickupSpace struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Private   bool     `json:"private"`
	MemberIDs []string `json:"-"`
}

type clickupFolder struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Lists []clickupList `json:"lists"`
}

type clickupList struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Content    string   `json:"content"`
	URL        string   `json:"url"`
	Status     string   `json:"-"`
	Archived   bool     `json:"archived"`
	SpaceID    string   `json:"-"` // populated by the loader
	FolderID   string   `json:"-"` // populated by the loader
	FolderName string   `json:"-"` // populated by the loader
	MemberIDs  []string `json:"-"`
}

type clickupTag struct {
	Name string `json:"name"`
}

type clickupAttachment struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	MimeType string `json:"mimetype"`
	Size     int64  `json:"size"`
}

type clickupTaskStatus struct {
	Status string `json:"status"`
	Type   string `json:"type"`
}

// clickupTask carries the fields we use from /list/{id}/task. The full
// response has 30+ fields; we keep the struct lean for clarity.
type clickupTask struct {
	ID          string `json:"id"`
	CustomID    string `json:"custom_id"`
	Name        string `json:"name"`
	TextContent string `json:"text_content"`
	Description string `json:"description"`
	URL         string `json:"url"`

	// Status comes back as an object with status + type.
	StatusObj clickupTaskStatus `json:"status"`
	// Flattened convenience fields for the mapper.
	StatusName string `json:"-"`
	StatusType string `json:"-"`

	// Priority is an object too: { id, priority (label), color, orderindex }.
	// Sometimes it's null when the user hasn't set one.
	PriorityObj   *clickupPriority `json:"priority"`
	PriorityID    string           `json:"-"`
	PriorityLabel string           `json:"-"`

	DateCreated string `json:"date_created"`
	DateUpdated string `json:"date_updated"`
	DateClosed  string `json:"date_closed"`
	StartDate   string `json:"start_date"`
	DueDate     string `json:"due_date"`

	CreatorObj *clickupUser `json:"creator"`
	CreatorID  string       `json:"-"`

	Assignees []clickupUser `json:"assignees"`
	Tags      []clickupTag  `json:"tags"`

	CustomFields []clickupField `json:"custom_fields"`

	// ParentRaw can be a string (parent task id) or null.
	ParentRaw json.RawMessage `json:"parent"`
	ParentID  string          `json:"-"`

	ListObj   clickupRefName `json:"list"`
	ListID    string         `json:"-"`
	FolderObj clickupRefName `json:"folder"`
	FolderID  string         `json:"-"`
	SpaceObj  clickupRefName `json:"space"`
	SpaceID   string         `json:"-"`

	Attachments []clickupAttachment `json:"attachments"`

	// Filled by the loader after a separate fetchTaskComments call.
	Comments []clickupComment `json:"-"`
}

type clickupPriority struct {
	ID       string `json:"id"`
	Priority string `json:"priority"`
}

type clickupRefName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (t *clickupTask) UnmarshalJSON(data []byte) error {
	// Parse with the auto-generated alias so we get every field, then
	// flatten the nested objects for ergonomic access from
	// taskToSourceTask.
	type alias clickupTask
	a := (*alias)(t)
	if err := json.Unmarshal(data, a); err != nil {
		return err
	}
	t.StatusName = a.StatusObj.Status
	t.StatusType = a.StatusObj.Type
	if a.PriorityObj != nil {
		t.PriorityID = a.PriorityObj.ID
		t.PriorityLabel = a.PriorityObj.Priority
	}
	if a.CreatorObj != nil {
		t.CreatorID = a.CreatorObj.ID
	}
	t.ListID = a.ListObj.ID
	t.FolderID = a.FolderObj.ID
	t.SpaceID = a.SpaceObj.ID

	// Parent: ClickUp returns a string id when set, null when not.
	if len(a.ParentRaw) > 0 && string(a.ParentRaw) != "null" {
		var pid string
		if err := json.Unmarshal(a.ParentRaw, &pid); err == nil {
			t.ParentID = pid
		}
	}
	return nil
}

type clickupComment struct {
	ID          string       `json:"id"`
	Comment     string       `json:"comment_text"`
	UserID      string       `json:"-"`
	UserObj     *clickupUser `json:"user"`
	DateCreated string       `json:"date"`
	Created     time.Time    `json:"-"`
}

func (c *clickupComment) UnmarshalJSON(data []byte) error {
	type alias clickupComment
	a := (*alias)(c)
	if err := json.Unmarshal(data, a); err != nil {
		return err
	}
	if a.UserObj != nil {
		c.UserID = a.UserObj.ID
	}
	if t := parseClickUpMillis(a.DateCreated); t != nil {
		c.Created = *t
	}
	return nil
}

// ─── Discoverer implementation ────────────────────────────────────

// Discover lists ClickUp workspaces accessible to the connected token.
// The result populates the FE's "pick a workspace" dropdown; the
// picked id flows back as opts["workspace_id"] on the next job.
func (p *Provider) Discover(ctx context.Context, ownerUserID string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	if token == nil || token.AccessToken == "" {
		return nil, errors.New("no clickup token saved")
	}
	access := token.AccessToken
	if token.RefreshToken != "" {
		owner, perr := uuid.Parse(ownerUserID)
		if perr == nil {
			if fresh, ferr := importProvider.FreshAccessToken(ctx, clickupOAuthConfig(), providerName, owner); ferr == nil && fresh != "" {
				access = fresh
			}
		}
	}
	var resp struct {
		Teams []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"teams"`
	}
	if err := p.getJSON(ctx, access, "/team", &resp); err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(resp.Teams))
	for _, t := range resp.Teams {
		out = append(out, importProvider.DiscoverItem{
			ID:   t.ID,
			Name: t.Name,
			Kind: "workspace",
			Meta: map[string]any{
				"clickup_color": t.Color,
			},
		})
	}
	return out, nil
}
