// Package todoist implements the Import Provider interface for Todoist.
//
// Source: live API only ("api"). Todoist's CSV/template export omits
// notes and assignees, so we don't surface it as a supported source.
//
// Auth: Bearer <token>. Either a Personal API token from
// https://todoist.com/app/settings/integrations/developer or an OAuth
// access token. Stored encrypted via importModels.SaveToken.
//
// Rate limits: 1000 requests / 15 minutes per user. The Sync API is one
// POST per import (the entire workspace fits in one response), so we're
// effectively under one request per import. The limiter exists for the
// per-comment attachment fetches.
//
// Mapping:
//
//	Workspace      → synthetic OneCamp team (orchestrator default).
//	Todoist project → OneCamp project.
//	Todoist section → OneCamp label on the task.
//	Item (parent_id="")    → top-level task.
//	Item (parent_id!="")   → subtask.
//	Note            → task comment.
//	File attachment → attachment.
//	Collaborator    → user.
//
// Priority inversion: Todoist API priority 1 is the LOWEST visually but
// returns as 1; priority 4 is the HIGHEST and returns as 4. We map
// 4→high, 3→medium, 2→low, 1→low for OneCamp's 3-level scale.
package todoist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const providerName = importModels.ProviderTodoist

// Provider implements the importProvider.Provider interface for Todoist.
type Provider struct {
	rl importProvider.Limiter
	mu sync.Mutex
	// snapshot caches the full sync response per import id. The Sync API
	// returns everything we need in one call, so each Iter* reads from
	// the same in-memory snapshot.
	snapshot map[uuid.UUID]*todoistSnapshot
}

func New() *Provider {
	return &Provider{
		// 1000/15min ≈ 1.1/s. Be conservative: 1 token per second, burst 30.
		rl:       importProvider.NewSleepLimiter(time.Second, 30),
		snapshot: make(map[uuid.UUID]*todoistSnapshot, 4),
	}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapProjects |
		importProvider.CapTasks |
		importProvider.CapSubtasks |
		importProvider.CapTaskComments |
		importProvider.CapAttachments
}

func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		"checked":   "done",
		"completed": "done",
		"active":    "todo",
		"open":      "todo",
	}
}

// DefaultPriorityMap matches Todoist's API priority (1..4 where 4 is
// highest) to OneCamp's 3-level scale.
func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"4": "high",
		"3": "medium",
		"2": "low",
		"1": "low",
	}
}

// Validate runs a tiny sync call to confirm the token is good.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	if _, err := p.sync(ctx, tok, []string{"user"}); err != nil {
		return fmt.Errorf("todoist auth: %w", err)
	}
	return nil
}

// Plan fetches the entire workspace, caches it on the provider, and
// returns aggregate counts. The cached snapshot is reused by every
// Iter* call for the same job id.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	snap, err := p.loadSnapshot(ctx, j)
	if err != nil {
		return nil, nil, err
	}

	plan := &importProvider.Plan{
		UserCount:    len(snap.Collaborators),
		ProjectCount: len(snap.activeProjects()),
	}
	for _, it := range snap.Items {
		if it.IsDeleted.on() {
			continue
		}
		if it.ParentID != "" {
			plan.SubtaskCount++
		} else {
			plan.TaskCount++
		}
	}
	for _, n := range snap.Notes {
		if n.IsDeleted.on() {
			continue
		}
		plan.CommentCount++
		if n.FileAttachment != nil && n.FileAttachment.FileURL != "" {
			plan.FileCount++
			plan.FileBytes += n.FileAttachment.FileSize
		}
	}

	plan.StatusValues = []string{"active", "checked"}
	plan.PriorityValues = []string{"1", "2", "3", "4"}

	if len(snap.activeProjects()) == 0 {
		plan.Warnings = append(plan.Warnings, "No active projects in this Todoist account")
	}

	return plan, nil, nil
}

// IterUsers streams collaborators[] from the cached snapshot.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("todoist.IterUsers", errCh)
		snap, err := p.loadSnapshot(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		// Always include the token-owning user (sync `user` block).
		if snap.User.ID != "" {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    snap.User.ID,
				DisplayName: snap.User.FullName,
				Login:       snap.User.Email,
				Email:       snap.User.Email,
				AvatarURL:   todoistAvatarURL(snap.User.ImageID),
			}:
			}
		}
		for _, c := range snap.Collaborators {
			if c.ID == snap.User.ID {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    c.ID,
				DisplayName: c.FullName,
				Login:       c.Email,
				Email:       c.Email,
				AvatarURL:   todoistAvatarURL(c.ImageID),
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits nothing — Todoist has no team tier. The orchestrator
// falls back to the synthetic default team.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam)
	errCh := make(chan error, 1)
	close(out)
	close(errCh)
	return out, errCh
}

// IterProjects streams active (non-archived, non-deleted) projects.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("todoist.IterProjects", errCh)
		snap, err := p.loadSnapshot(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		// Members = collaborators of the project. The Sync API has
		// project_collaborators[] (from a separate sync slice). To keep
		// the implementation scoped, we treat the workspace's full
		// collaborators list as the project's member set; per-project
		// permission filtering can be a follow-up.
		members := make([]string, 0, len(snap.Collaborators))
		for _, c := range snap.Collaborators {
			if c.IsDeleted.on() {
				continue
			}
			members = append(members, c.ID)
		}
		if snap.User.ID != "" {
			members = append(members, snap.User.ID)
		}

		for _, pj := range snap.activeProjects() {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:  pj.ID,
				Name:      pj.Name,
				MemberIds: members,
				AdminIds:  nil,
				Created:   time.Time{},
				Archived:  pj.IsArchived.on(),
				Metadata:  map[string]any{"color": pj.Color, "parent_id": pj.ParentID},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams non-deleted, top-level items for one project.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("todoist.IterTasksOfProject", errCh)
		snap, err := p.loadSnapshot(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		// Section name lookup so we can attach section labels.
		sectionName := map[string]string{}
		for _, s := range snap.Sections {
			if s.IsDeleted.on() {
				continue
			}
			sectionName[s.ID] = s.Name
		}

		for _, it := range snap.Items {
			if it.IsDeleted.on() || it.ProjectID != projectSourceId {
				continue
			}
			if it.ParentID != "" {
				continue // subtasks handled by IterSubtasksOfTask
			}
			select {
			case <-ctx.Done():
				return
			case out <- buildSourceTask(snap, &it, sectionName):
			}
		}
		// Emit synthetic subtask-parent placeholders so the orchestrator
		// schedules subtask chunks for any task that has children.
		seen := map[string]struct{}{}
		for _, it := range snap.Items {
			if it.IsDeleted.on() || it.ProjectID != projectSourceId {
				continue
			}
			if it.ParentID == "" {
				continue
			}
			if _, dup := seen[it.ParentID]; dup {
				continue
			}
			seen[it.ParentID] = struct{}{}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceTask{
				SourceID:     "__sub__:" + it.ParentID,
				ParentTaskID: it.ParentID,
			}:
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask streams items whose parent_id matches.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("todoist.IterSubtasksOfTask", errCh)
		snap, err := p.loadSnapshot(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		sectionName := map[string]string{}
		for _, s := range snap.Sections {
			if !s.IsDeleted.on() {
				sectionName[s.ID] = s.Name
			}
		}
		for _, it := range snap.Items {
			if it.IsDeleted.on() || it.ParentID != taskSourceId {
				continue
			}
			task := buildSourceTask(snap, &it, sectionName)
			task.ParentTaskID = taskSourceId
			select {
			case <-ctx.Done():
				return
			case out <- task:
			}
		}
	}()
	return out, errCh
}

// IterCommentsOfTask streams notes whose item_id matches. Each note's
// file_attachment becomes a SourceAttachment.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("todoist.IterCommentsOfTask", errCh)
		snap, err := p.loadSnapshot(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		for _, n := range snap.Notes {
			if n.IsDeleted.on() || n.ItemID != taskSourceId {
				continue
			}
			created, _ := time.Parse(time.RFC3339, n.PostedAt)
			body := todoistMarkdownToHTML(n.Content)
			atts := []importProvider.SourceAttachment{}
			if n.FileAttachment != nil && n.FileAttachment.FileURL != "" {
				atts = append(atts, importProvider.SourceAttachment{
					SourceID: n.ID + "@file",
					Name:     n.FileAttachment.FileName,
					URL:      n.FileAttachment.FileURL,
					Mime:     n.FileAttachment.FileType,
					Size:     n.FileAttachment.FileSize,
					Parent:   importProvider.SourceRef{Kind: "comment", SourceID: n.ID},
				})
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceComment{
				SourceID:       n.ID,
				TaskSourceID:   taskSourceId,
				Body:           body,
				AuthorSourceID: n.PostedUID,
				Created:        created,
				AttachmentRefs: atts,
			}:
			}
		}
	}()
	return out, errCh
}

// FetchAttachment downloads via plain HTTPS — Todoist file URLs are
// public S3 links and do not need the Bearer header.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	return importProvider.DefaultFetchAttachment(ctx, att, dest)
}

// ─── snapshot + sync ──────────────────────────────────────────────

type todoistSnapshot struct {
	User          todoistUser      `json:"user"`
	Projects      []todoistProject `json:"projects"`
	Sections      []todoistSection `json:"sections"`
	Items         []todoistItem    `json:"items"`
	Notes         []todoistNote    `json:"notes"`
	Collaborators []todoistCollab  `json:"collaborators"`
	Labels        []todoistLabel   `json:"labels"`
	SyncToken     string           `json:"sync_token"`
}

func (s *todoistSnapshot) activeProjects() []todoistProject {
	out := make([]todoistProject, 0, len(s.Projects))
	for _, p := range s.Projects {
		if p.IsDeleted.on() {
			continue
		}
		out = append(out, p)
	}
	return out
}

type todoistUser struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	ImageID  string `json:"image_id"`
}

type todoistProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Color      string `json:"color"`
	ParentID   string `json:"parent_id"`
	IsArchived flag   `json:"is_archived"`
	IsDeleted  flag   `json:"is_deleted"`
}

type todoistSection struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	IsDeleted flag   `json:"is_deleted"`
}

type todoistItem struct {
	ID             string   `json:"id"`
	Content        string   `json:"content"`
	Description    string   `json:"description"`
	ProjectID      string   `json:"project_id"`
	SectionID      string   `json:"section_id"`
	ParentID       string   `json:"parent_id"`
	Priority       int      `json:"priority"`
	Labels         []string `json:"labels"`
	AssignedByUID  string   `json:"assigned_by_uid"`
	ResponsibleUID string   `json:"responsible_uid"`
	Checked        flag     `json:"checked"`
	IsDeleted      flag     `json:"is_deleted"`
	AddedAt        string   `json:"added_at"`
	CompletedAt    string   `json:"completed_at"`
	UpdatedAt      string   `json:"updated_at"`
	Due            *struct {
		Date        string `json:"date"` // YYYY-MM-DD or RFC3339
		IsRecurring bool   `json:"is_recurring"`
	} `json:"due"`
}

type todoistNote struct {
	ID             string `json:"id"`
	ItemID         string `json:"item_id"`
	Content        string `json:"content"`
	PostedUID      string `json:"posted_uid"`
	PostedAt       string `json:"posted_at"`
	IsDeleted      flag   `json:"is_deleted"`
	FileAttachment *struct {
		FileName string `json:"file_name"`
		FileSize int64  `json:"file_size"`
		FileType string `json:"file_type"`
		FileURL  string `json:"file_url"`
	} `json:"file_attachment"`
}

type todoistCollab struct {
	ID        string `json:"id"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
	ImageID   string `json:"image_id"`
	IsDeleted flag   `json:"is_deleted"`
}

type todoistLabel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDeleted flag   `json:"is_deleted"`
}

// loadSnapshot fetches the full workspace once per job and caches it.
// Concurrent callers see a consistent snapshot.
func (p *Provider) loadSnapshot(ctx context.Context, j *importModels.Job) (*todoistSnapshot, error) {
	p.mu.Lock()
	if cached, ok := p.snapshot[j.Id]; ok {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}
	snap, err := p.sync(ctx, tok, []string{
		"user", "projects", "sections", "items", "notes",
		"collaborators", "labels",
	})
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.snapshot[j.Id] = snap
	p.mu.Unlock()
	return snap, nil
}

// syncURL is Todoist API v1's sync endpoint. Sync API v9
// (/sync/v9/sync) was shut down in 2026; v1 kept the endpoint's shape
// and its resource names ("items", "notes"). A var so tests can point it
// at a fake.
var syncURL = "https://api.todoist.com/api/v1/sync"

// flag is a Todoist yes/no: true/false in API v1, 0/1 in older answers.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "true", "1":
		*f = true
	default:
		*f = false
	}
	return nil
}

func (f flag) on() bool { return bool(f) }

// sync calls the single Todoist Sync API endpoint with the requested
// resource_types. For our use case sync_token="*" returns everything;
// future incremental syncs would persist the returned token.
func (p *Provider) sync(ctx context.Context, tok string, resourceTypes []string) (*todoistSnapshot, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return nil, err
	}
	body := url.Values{}
	body.Set("sync_token", "*")
	rt, _ := json.Marshal(resourceTypes)
	body.Set("resource_types", string(rt))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syncURL,
		bytes.NewBufferString(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 30 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, &importProvider.ErrRateLimited{RetryAfter: retryAfter, Reason: "Todoist 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("todoist auth failed (HTTP %d); reconnect token", resp.StatusCode)
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("todoist HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var s todoistSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode todoist sync: %w", err)
	}
	return &s, nil
}

// loadToken returns the access token bytes. Todoist needs no companion
// api_key field, so we read access_token directly.
func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty todoist token")
	}
	return t.AccessToken, nil
}

// ─── helpers ──────────────────────────────────────────────────────

// buildSourceTask converts a todoistItem to the generic SourceTask
// used by the orchestrator.
func buildSourceTask(snap *todoistSnapshot, it *todoistItem, sectionName map[string]string) importProvider.SourceTask {
	addedAt, _ := time.Parse(time.RFC3339, it.AddedAt)
	updatedAt, _ := time.Parse(time.RFC3339, it.UpdatedAt)
	var due *time.Time
	if it.Due != nil && it.Due.Date != "" {
		if t, err := time.Parse(time.RFC3339, it.Due.Date); err == nil {
			due = &t
		} else if t, err := time.Parse("2006-01-02", it.Due.Date); err == nil {
			due = &t
		}
	}

	status := "active"
	if it.Checked.on() || it.CompletedAt != "" {
		status = "checked"
	}

	labels := append([]string{}, it.Labels...)
	if name := sectionName[it.SectionID]; name != "" {
		labels = append(labels, "section:"+name)
	}

	assignees := []string{}
	if it.ResponsibleUID != "" {
		assignees = append(assignees, it.ResponsibleUID)
	}

	body := todoistMarkdownToHTML(it.Description)

	return importProvider.SourceTask{
		SourceID:        it.ID,
		ProjectSourceID: it.ProjectID,
		Name:            it.Content,
		Description:     body,
		Status:          status,
		Priority:        strconv.Itoa(it.Priority),
		Labels:          labels,
		AssigneeIds:     assignees,
		DueDate:         due,
		Created:         addedAt,
		Updated:         updatedAt,
		Completed:       status == "checked",
		Metadata:        map[string]any{"todoist_url": "https://todoist.com/showTask?id=" + it.ID},
	}
}

// todoistMarkdownToHTML produces a tiny HTML rendering of Todoist's
// markdown subset. We handle paragraphs, **bold**, *italic*, links,
// inline `code`, and line breaks. Headings/lists are rare in Todoist
// task descriptions; if needed the operator sees the raw markdown via
// the import error log.
func todoistMarkdownToHTML(s string) string {
	if s == "" {
		return ""
	}
	out := htmlEscape(s)
	// Links: [text](url)
	out = mdLinkRe.ReplaceAllString(out, `<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>`)
	// **bold**
	out = mdBoldRe.ReplaceAllString(out, `<strong>$1</strong>`)
	// *italic* (greedy enough for typical content)
	out = mdItalicRe.ReplaceAllString(out, `<em>$1</em>`)
	// `code`
	out = mdCodeRe.ReplaceAllString(out, `<code>$1</code>`)
	// Newlines → <br>.
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' {
			out = out[:i] + "<br>" + out[i+1:]
			i += 3
		}
	}
	return out
}

// todoistAvatarURL builds the public avatar URL given an image_id.
// Todoist serves avatars at https://dcff1xvirvpfp.cloudfront.net/<id>_big.jpg.
// If the id is empty, return empty so the user gets initials.
func todoistAvatarURL(imageID string) string {
	if imageID == "" {
		return ""
	}
	return "https://dcff1xvirvpfp.cloudfront.net/" + imageID + "_big.jpg"
}

func htmlEscape(s string) string {
	rep := []struct{ from, to string }{
		{"&", "&amp;"},
		{"<", "&lt;"},
		{">", "&gt;"},
		{`"`, "&quot;"},
		{"'", "&#39;"},
	}
	for _, r := range rep {
		s = stringReplaceAll(s, r.from, r.to)
	}
	return s
}

func stringReplaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	var b []byte
	for {
		i := bytesIndex(s, old)
		if i < 0 {
			break
		}
		b = append(b, s[:i]...)
		b = append(b, new...)
		s = s[i+len(old):]
	}
	return string(append(b, s...))
}

func bytesIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Pre-compiled markdown regexes. Kept package-level so each task
// description doesn't pay the compile cost.
var (
	mdLinkRe   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	mdBoldRe   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdItalicRe = regexp.MustCompile(`(?:^|[^*])\*([^*]+)\*`)
	mdCodeRe   = regexp.MustCompile("`([^`]+)`")
)

// CleanupJob evicts the cached snapshot for a finished job.
// Implements importProvider.JobCleaner.
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	p.mu.Lock()
	delete(p.snapshot, id)
	p.mu.Unlock()
}
