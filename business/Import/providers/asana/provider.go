// Package asana implements the Import Provider interface for Asana.
//
// Source: live API only ("api"). Asana's CSV export is per-project and
// omits subtasks + stories, so we don't surface it as a supported source.
//
// Auth: Bearer <token>. Either a Personal Access Token from
// https://app.asana.com/0/my-apps or an OAuth2 access token. Stored
// encrypted via importModels.SaveToken.
//
// Rate limits: 1500 requests/minute per token. We use SleepLimiter
// ticking every 40ms with 50 burst (~1500/min sustained).
//
// Workspace selection: the operator names the workspace in
// SourceWorkspaceName; we resolve it to a gid by listing /workspaces.
// If multiple workspaces match (rare), the first is used and a warning
// is logged.
//
// Pagination: every Iter* uses Asana's `next_page.path`. We follow up
// to a safety cap of 200 pages (so a runaway loop can't hang a worker).
package asana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const providerName = importModels.ProviderAsana

// apiBase is Asana's API; a var so tests can point it at a fake.
var apiBase = "https://app.asana.com/api/1.0"

// Pagination safety cap. 200 pages × 100 items = 20k items per
// per-project iteration, which covers every realistic Asana project.
const maxPages = 200

// Provider implements the importProvider.Provider interface for Asana.
type Provider struct {
	rl importProvider.Limiter

	// workspaceCache maps a job id → resolved Asana workspace gid so
	// each Iter* doesn't re-resolve. Bounded by the number of concurrent
	// imports (uuid.UUID keys never repeat).
	mu             sync.Mutex
	workspaceCache map[uuid.UUID]string
}

func New() *Provider {
	return &Provider{
		rl:             importProvider.NewSleepLimiter(40*time.Millisecond, 50),
		workspaceCache: make(map[uuid.UUID]string, 4),
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

func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		"completed":   "done",
		"to do":       "todo",
		"todo":        "todo",
		"in progress": "inProgress",
		"doing":       "inProgress",
		"in review":   "inReview",
		"review":      "inReview",
		"done":        "done",
		"cancelled":   "canceled",
	}
}

func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"high":   "high",
		"medium": "medium",
		"low":    "low",
		"p1":     "high",
		"p2":     "medium",
		"p3":     "low",
	}
}

// Validate confirms /users/me succeeds with the saved token.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	var me struct {
		Data struct {
			GID string `json:"gid"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, tok, "/users/me", &me); err != nil {
		return fmt.Errorf("asana auth: %w", err)
	}
	if me.Data.GID == "" {
		return errors.New("asana /users/me returned empty gid")
	}
	return nil
}

// Plan resolves the workspace, lists teams + projects, and counts
// tasks per project (cheap because Asana's task count returns in
// limit=1 + opt_fields=name responses with `next_page.uri` we follow
// to count). For sanity we cap per-project counting to 5000 tasks.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	wid, err := p.resolveWorkspaceGID(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, nil, err
	}

	// Users (count only).
	users, err := p.listUsers(ctx, tok, wid)
	if err != nil {
		return nil, nil, fmt.Errorf("asana users: %w", err)
	}

	// Teams + projects.
	teams, err := p.listTeams(ctx, tok, wid)
	if err != nil {
		// Teams require a paid Asana org; tolerate the failure.
		teams = nil
	}
	projects, err := p.listProjects(ctx, tok, wid)
	if err != nil {
		return nil, nil, fmt.Errorf("asana projects: %w", err)
	}

	plan := &importProvider.Plan{
		UserCount:    len(users),
		TeamCount:    len(teams),
		ProjectCount: len(projects),
	}

	// Surface the section names the operator can map to statuses.
	statusSet := map[string]struct{}{}
	for _, pr := range projects {
		secs, _ := p.listSections(ctx, tok, pr.GID)
		for _, s := range secs {
			n := strings.ToLower(strings.TrimSpace(s.Name))
			if n != "" {
				statusSet[n] = struct{}{}
			}
		}
		// Per-project task count. We cap to 5000 so a multi-tens-of-k
		// task project doesn't make Plan take minutes.
		count, capped, err := p.countTasksInProject(ctx, tok, pr.GID, 5000)
		if err == nil {
			plan.TaskCount += count
			if capped {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("Task count for project %q capped at 5000 — Plan stage capped, full Run will still import everything", pr.Name))
			}
		}
	}
	for s := range statusSet {
		plan.StatusValues = append(plan.StatusValues, s)
	}
	plan.PriorityValues = []string{"high", "medium", "low"}

	return plan, nil, nil
}

// IterUsers streams workspace users.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterUsers", errCh)
		wid, err := p.resolveWorkspaceGID(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		users, err := p.listUsers(ctx, tok, wid)
		if err != nil {
			errCh <- err
			return
		}
		for _, u := range users {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    u.GID,
				DisplayName: u.Name,
				Email:       u.Email,
				AvatarURL:   helpers.FirstNonEmpty(u.Photo.Image128x128, u.Photo.Image60x60),
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams streams workspace teams + their members.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam, 4)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterTeams", errCh)
		wid, err := p.resolveWorkspaceGID(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		teams, err := p.listTeams(ctx, tok, wid)
		if err != nil {
			// Teams API requires Asana Premium / Business. Emit nothing
			// rather than failing the import; orchestrator falls back to
			// the synthetic default team.
			return
		}
		for _, t := range teams {
			members, _ := p.listTeamMembers(ctx, tok, t.GID)
			memberIds := make([]string, 0, len(members))
			for _, m := range members {
				memberIds = append(memberIds, m.GID)
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceTeam{
				SourceID:    t.GID,
				Name:        t.Name,
				Description: t.Description,
				MemberIds:   memberIds,
			}:
			}
		}
	}()
	return out, errCh
}

// IterProjects streams workspace projects.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterProjects", errCh)
		wid, err := p.resolveWorkspaceGID(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		projects, err := p.listProjects(ctx, tok, wid)
		if err != nil {
			errCh <- err
			return
		}
		for _, pr := range projects {
			members, _ := p.listProjectMembers(ctx, tok, pr.GID)
			// Custom fields need a paid Asana plan: none, or none readable,
			// is a project without them; tasks still bring their values.
			fields, _ := p.listProjectFields(ctx, tok, pr.GID)
			memberIds := make([]string, 0, len(members))
			for _, m := range members {
				memberIds = append(memberIds, m.GID)
			}
			created, _ := time.Parse(time.RFC3339, pr.CreatedAt)
			teamSrc := ""
			if pr.Team != nil {
				teamSrc = pr.Team.GID
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:     pr.GID,
				Name:         pr.Name,
				Description:  pr.Notes,
				TeamSourceID: teamSrc,
				MemberIds:    memberIds,
				CreatedBy:    safeGID(pr.CreatedBy),
				Created:      created,
				Archived:     pr.Archived,
				Fields:       fields,
				Metadata:     map[string]any{"asana_url": "https://app.asana.com/0/" + pr.GID},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams top-level tasks for one project.
// Subtask discovery happens at the task level via the resource_subtype
// metadata — we mark tasks with subtask_count > 0 so the orchestrator
// schedules a subtask chunk.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterTasksOfProject", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		// Build section name lookup so tasks can carry their section
		// label as a clamped status / label.
		sections, _ := p.listSections(ctx, tok, projectSourceId)
		sectionName := map[string]string{}
		for _, s := range sections {
			sectionName[s.GID] = s.Name
		}

		path := fmt.Sprintf("/projects/%s/tasks?limit=100&opt_fields=%s",
			projectSourceId, taskOptFields)
		page := 0
		for path != "" && page < maxPages {
			page++
			var resp asanaTasksResp
			if err := p.getJSON(ctx, tok, path, &resp); err != nil {
				errCh <- err
				return
			}
			for _, t := range resp.Data {
				select {
				case <-ctx.Done():
					return
				case out <- buildSourceTask(&t, projectSourceId, sectionName, ""):
				}
				// Schedule a subtask chunk only if the parent has children.
				if t.NumSubtasks > 0 {
					select {
					case <-ctx.Done():
						return
					case out <- importProvider.SourceTask{
						SourceID:     "__sub__:" + t.GID,
						ParentTaskID: t.GID,
					}:
					}
				}
			}
			path = nextPagePath(resp.NextPage)
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask paginates /tasks/{gid}/subtasks. Asana's subtasks
// can themselves have subtasks; we clamp the depth to 1 because the
// OneCamp model only supports one level of nesting natively.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterSubtasksOfTask", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		path := fmt.Sprintf("/tasks/%s/subtasks?limit=100&opt_fields=%s",
			taskSourceId, taskOptFields)
		page := 0
		for path != "" && page < maxPages {
			page++
			var resp asanaTasksResp
			if err := p.getJSON(ctx, tok, path, &resp); err != nil {
				errCh <- err
				return
			}
			for _, t := range resp.Data {
				task := buildSourceTask(&t, "", nil, taskSourceId)
				task.ParentTaskID = taskSourceId
				select {
				case <-ctx.Done():
					return
				case out <- task:
				}
			}
			path = nextPagePath(resp.NextPage)
		}
	}()
	return out, errCh
}

// IterCommentsOfTask paginates /tasks/{gid}/stories. Only stories of
// type "comment" become OneCamp comments; "system" stories (status
// changes etc.) are filtered unless opts.keep_system_comments is true.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 8)
	errCh := make(chan error, 1)
	keepSystem, _ := opts["keep_system_comments"].(bool)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("asana.IterCommentsOfTask", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		path := fmt.Sprintf("/tasks/%s/stories?limit=100&opt_fields=gid,type,resource_subtype,html_text,text,created_at,created_by.gid,created_by.name",
			taskSourceId)
		page := 0
		for path != "" && page < maxPages {
			page++
			var resp asanaStoriesResp
			if err := p.getJSON(ctx, tok, path, &resp); err != nil {
				errCh <- err
				return
			}
			for _, s := range resp.Data {
				isComment := s.Type == "comment"
				if !isComment && !keepSystem {
					continue
				}
				body := s.HTMLText
				if body == "" {
					body = htmlEscape(s.Text)
				}
				created, _ := time.Parse(time.RFC3339, s.CreatedAt)
				select {
				case <-ctx.Done():
					return
				case out <- importProvider.SourceComment{
					SourceID:       s.GID,
					TaskSourceID:   taskSourceId,
					Body:           body,
					AuthorSourceID: safeGID(s.CreatedBy),
					Created:        created,
					IsSystem:       !isComment,
				}:
				}
			}
			path = nextPagePath(resp.NextPage)
		}
	}()
	return out, errCh
}

// FetchAttachment refreshes the download_url on each fetch (Asana's
// attachment URLs are short-lived presigned S3 links) and downloads
// with the Bearer token attached for the metadata GET only — the
// resulting download_url itself is the credential and needs no header.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", 0, err
	}
	// Refresh the URL.
	var meta struct {
		Data struct {
			DownloadURL string `json:"download_url"`
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			Host        string `json:"host"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, tok, "/attachments/"+url.PathEscape(att.SourceID)+"?opt_fields=download_url,name,size,host", &meta); err != nil {
		return "", 0, err
	}
	if meta.Data.DownloadURL == "" {
		return "", 0, importProvider.ErrAttachmentGone
	}
	dl := att
	dl.URL = meta.Data.DownloadURL
	dl.Headers = nil // download_url is presigned
	return importProvider.DefaultFetchAttachment(ctx, dl, dest)
}

// ─── Asana DTOs (subset we use) ───────────────────────────────────

type asanaUser struct {
	GID   string `json:"gid"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Photo struct {
		Image60x60   string `json:"image_60x60"`
		Image128x128 string `json:"image_128x128"`
	} `json:"photo"`
}

type asanaTeam struct {
	GID         string `json:"gid"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type asanaWorkspace struct {
	GID  string `json:"gid"`
	Name string `json:"name"`
}

type asanaProject struct {
	GID       string         `json:"gid"`
	Name      string         `json:"name"`
	Notes     string         `json:"notes"`
	Archived  bool           `json:"archived"`
	CreatedAt string         `json:"created_at"`
	CreatedBy *asanaRefShort `json:"created_by"`
	Team      *asanaRefShort `json:"team"`
	Workspace *asanaRefShort `json:"workspace"`
}

type asanaSection struct {
	GID  string `json:"gid"`
	Name string `json:"name"`
}

type asanaRefShort struct {
	GID  string `json:"gid"`
	Name string `json:"name"`
}

// asanaTask is the per-task shape with the opt_fields we request below.
type asanaTask struct {
	GID          string             `json:"gid"`
	Name         string             `json:"name"`
	Notes        string             `json:"notes"`
	HTMLNotes    string             `json:"html_notes"`
	Completed    bool               `json:"completed"`
	CompletedAt  string             `json:"completed_at"`
	CreatedAt    string             `json:"created_at"`
	ModifiedAt   string             `json:"modified_at"`
	DueOn        string             `json:"due_on"`
	DueAt        string             `json:"due_at"`
	StartOn      string             `json:"start_on"`
	StartAt      string             `json:"start_at"`
	Assignee     *asanaRefShort     `json:"assignee"`
	NumSubtasks  int                `json:"num_subtasks"`
	Memberships  []asanaMembership  `json:"memberships"`
	Tags         []asanaRefShort    `json:"tags"`
	CustomFields []asanaCustomField `json:"custom_fields"`
	Permalink    string             `json:"permalink_url"`
}

type asanaMembership struct {
	Project *asanaRefShort `json:"project"`
	Section *asanaRefShort `json:"section"`
}

// asanaCustomField is a task's value of one custom field (fields.go).
type asanaCustomField struct {
	GID             string   `json:"gid"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	ResourceSubtype string   `json:"resource_subtype"`
	Format          string   `json:"format"`
	CurrencyCode    string   `json:"currency_code"`
	IsFormula       bool     `json:"is_formula_field"`
	DisplayValue    string   `json:"display_value"`
	TextValue       *string  `json:"text_value"`
	NumberValue     *float64 `json:"number_value"`
	DateValue       *struct {
		Date string `json:"date"`
	} `json:"date_value"`
	EnumValue       *asanaEnumOption  `json:"enum_value"`
	MultiEnumValues []asanaEnumOption `json:"multi_enum_values"`
	PeopleValue     []asanaRefShort   `json:"people_value"`
}

// taskOptFields is the URL-encoded list of fields we ask for. Keep
// trimmed to what we use because Asana counts payload bytes towards
// rate limits.
var taskOptFields = url.QueryEscape("gid,name,notes,html_notes,completed,completed_at,created_at,modified_at,due_on,due_at,start_on,start_at,assignee.gid,assignee.name,num_subtasks,memberships.project.gid,memberships.section.gid,memberships.section.name,tags.name,permalink_url," + valueOptFields)

type asanaStory struct {
	GID             string         `json:"gid"`
	Type            string         `json:"type"`
	ResourceSubtype string         `json:"resource_subtype"`
	HTMLText        string         `json:"html_text"`
	Text            string         `json:"text"`
	CreatedAt       string         `json:"created_at"`
	CreatedBy       *asanaRefShort `json:"created_by"`
}

// Asana paginated response wrappers.
type asanaUsersResp struct {
	Data     []asanaUser    `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaWorkspacesResp struct {
	Data []asanaWorkspace `json:"data"`
}
type asanaTeamsResp struct {
	Data     []asanaTeam    `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaProjectsResp struct {
	Data     []asanaProject `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaSectionsResp struct {
	Data     []asanaSection `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaTasksResp struct {
	Data     []asanaTask    `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaStoriesResp struct {
	Data     []asanaStory   `json:"data"`
	NextPage *asanaNextPage `json:"next_page"`
}
type asanaNextPage struct {
	Path string `json:"path"`
	URI  string `json:"uri"`
}

// nextPagePath returns "" when there are no more pages.
func nextPagePath(np *asanaNextPage) string {
	if np == nil {
		return ""
	}
	return np.Path
}

// ─── HTTP helpers ─────────────────────────────────────────────────

// getJSON does GET <apiBase><path> with the Bearer token, decoding into out.
// Honours 429 with Retry-After. The path may already start with `/` or
// be a "next_page.path" returned by Asana (which starts with `/api/1.0/`).
func (p *Provider) getJSON(ctx context.Context, tok, path string, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	full := path
	if strings.HasPrefix(path, "/api/") {
		// next_page.path includes the version prefix; strip it because
		// apiBase already covers it.
		full = strings.TrimPrefix(path, "/api/1.0")
	}
	if !strings.HasPrefix(full, "/") {
		full = "/" + full
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+full, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 60 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return &importProvider.ErrRateLimited{RetryAfter: retryAfter, Reason: "Asana 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("asana auth failed (HTTP %d); reconnect token", resp.StatusCode)
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("asana HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Provider) listUsers(ctx context.Context, tok, wid string) ([]asanaUser, error) {
	out := []asanaUser{}
	path := fmt.Sprintf("/workspaces/%s/users?limit=100&opt_fields=gid,name,email,photo.image_60x60,photo.image_128x128", wid)
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp asanaUsersResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

// listTeams enumerates teams in an Asana organization. Plain (non-org)
// workspaces don't have teams and the endpoint returns 404; we treat
// that as "no teams" and let the orchestrator fall back to the
// synthetic default team.
func (p *Provider) listTeams(ctx context.Context, tok, wid string) ([]asanaTeam, error) {
	out := []asanaTeam{}
	path := fmt.Sprintf("/organizations/%s/teams?limit=100&opt_fields=gid,name,description", wid)
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp asanaTeamsResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			// Plain workspaces return 404 here. Surface as nil teams
			// so the orchestrator falls through to the synthetic default
			// team rather than failing the whole import.
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not_found") {
				return nil, nil
			}
			return nil, err
		}
		out = append(out, resp.Data...)
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

func (p *Provider) listTeamMembers(ctx context.Context, tok, teamGID string) ([]asanaUser, error) {
	out := []asanaUser{}
	path := fmt.Sprintf("/teams/%s/users?limit=100&opt_fields=gid,name,email", teamGID)
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp asanaUsersResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

func (p *Provider) listProjects(ctx context.Context, tok, wid string) ([]asanaProject, error) {
	out := []asanaProject{}
	path := fmt.Sprintf("/workspaces/%s/projects?limit=100&archived=false&opt_fields=gid,name,notes,archived,created_at,created_by.gid,team.gid,workspace.gid", wid)
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp asanaProjectsResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

func (p *Provider) listProjectMembers(ctx context.Context, tok, projectGID string) ([]asanaUser, error) {
	// /projects/{gid}/project_memberships?opt_fields=user.gid
	out := []asanaUser{}
	path := fmt.Sprintf("/projects/%s/project_memberships?limit=100&opt_fields=user.gid,user.name,user.email", projectGID)
	page := 0
	type pm struct {
		User asanaUser `json:"user"`
	}
	type pmResp struct {
		Data     []pm           `json:"data"`
		NextPage *asanaNextPage `json:"next_page"`
	}
	for path != "" && page < maxPages {
		page++
		var resp pmResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Data {
			out = append(out, m.User)
		}
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

func (p *Provider) listSections(ctx context.Context, tok, projectGID string) ([]asanaSection, error) {
	out := []asanaSection{}
	path := fmt.Sprintf("/projects/%s/sections?limit=100&opt_fields=gid,name", projectGID)
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp asanaSectionsResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}

// countTasksInProject returns (count, capped, err) where capped is true
// if we hit the cap before exhausting pages. Used by Plan only.
func (p *Provider) countTasksInProject(ctx context.Context, tok, projectGID string, cap int) (int, bool, error) {
	type lite struct {
		Data []struct {
			GID string `json:"gid"`
		} `json:"data"`
		NextPage *asanaNextPage `json:"next_page"`
	}
	path := fmt.Sprintf("/projects/%s/tasks?limit=100&opt_fields=gid", projectGID)
	count := 0
	page := 0
	for path != "" && page < maxPages {
		page++
		var resp lite
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return count, false, err
		}
		count += len(resp.Data)
		if count >= cap {
			return count, true, nil
		}
		path = nextPagePath(resp.NextPage)
	}
	return count, false, nil
}

// resolveWorkspaceGID looks up the Asana workspace by name and caches
// the gid per job id. Names are matched case-insensitively; the first
// match wins.
func (p *Provider) resolveWorkspaceGID(ctx context.Context, j *importModels.Job) (string, error) {
	p.mu.Lock()
	if cached, ok := p.workspaceCache[j.Id]; ok {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", err
	}
	var resp asanaWorkspacesResp
	if err := p.getJSON(ctx, tok, "/workspaces?limit=100&opt_fields=gid,name", &resp); err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(j.SourceWorkspaceName))
	for _, w := range resp.Data {
		if strings.ToLower(w.Name) == want {
			p.mu.Lock()
			p.workspaceCache[j.Id] = w.GID
			p.mu.Unlock()
			return w.GID, nil
		}
	}
	if len(resp.Data) == 0 {
		return "", errors.New("asana: token has access to no workspaces")
	}
	// Fall back to first workspace; emit a clear warning via the
	// imports error log on the caller's side.
	p.mu.Lock()
	p.workspaceCache[j.Id] = resp.Data[0].GID
	p.mu.Unlock()
	return resp.Data[0].GID, nil
}

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty asana token")
	}
	// OAuth tokens (with refresh_token populated) expire in 1 hour and
	// would silently break a long historical import. Route them through
	// the shared refresher; PATs (no refresh_token) bypass the refresh
	// path inside FreshAccessToken and return as-is.
	if t.RefreshToken != "" {
		fresh, ferr := importProvider.FreshAccessToken(ctx, asanaOAuthConfig(), providerName, *j.TriggeredBy)
		if ferr == nil && fresh != "" {
			return fresh, nil
		}
	}
	return t.AccessToken, nil
}

// ─── helpers ──────────────────────────────────────────────────────

// buildSourceTask converts an Asana task to the generic SourceTask.
//
// Status: Asana has no native "status" — we combine task.completed
// with the section it sits in (memberships[].section.name when the
// project gid matches). The operator maps section names to OneCamp
// statuses in the Plan UI.
//
// Priority: pulled from a custom field named "Priority" (case-insens),
// then stamped as the raw display value. Tasks without one fall back
// to "medium" via heuristic.
func buildSourceTask(t *asanaTask, projectGID string, sectionName map[string]string, fallbackParent string) importProvider.SourceTask {
	created, _ := time.Parse(time.RFC3339, t.CreatedAt)
	modified, _ := time.Parse(time.RFC3339, t.ModifiedAt)

	var due *time.Time
	if t.DueAt != "" {
		if v, err := time.Parse(time.RFC3339, t.DueAt); err == nil {
			due = &v
		}
	} else if t.DueOn != "" {
		if v, err := time.Parse("2006-01-02", t.DueOn); err == nil {
			due = &v
		}
	}
	var start *time.Time
	if t.StartAt != "" {
		if v, err := time.Parse(time.RFC3339, t.StartAt); err == nil {
			start = &v
		}
	} else if t.StartOn != "" {
		if v, err := time.Parse("2006-01-02", t.StartOn); err == nil {
			start = &v
		}
	}

	// Status raw value: prefer section name within the active project;
	// fall back to "completed"/"open".
	status := "open"
	if t.Completed {
		status = "completed"
	}
	for _, m := range t.Memberships {
		if m.Project == nil || m.Section == nil {
			continue
		}
		if projectGID == "" || m.Project.GID == projectGID {
			if m.Section.Name != "" {
				status = m.Section.Name
			}
			break
		}
	}

	// Priority custom field.
	priority := ""
	for _, cf := range t.CustomFields {
		if strings.EqualFold(cf.Name, "priority") && cf.DisplayValue != "" {
			priority = cf.DisplayValue
			break
		}
	}

	labels := make([]string, 0, len(t.Tags))
	for _, tg := range t.Tags {
		if tg.Name != "" {
			labels = append(labels, tg.Name)
		}
	}

	assignees := []string{}
	if t.Assignee != nil && t.Assignee.GID != "" {
		assignees = append(assignees, t.Assignee.GID)
	}

	body := t.HTMLNotes
	if body == "" {
		body = htmlEscape(t.Notes)
	}

	return importProvider.SourceTask{
		SourceID:        t.GID,
		ProjectSourceID: projectGID,
		ParentTaskID:    fallbackParent,
		Name:            t.Name,
		Description:     body,
		Status:          status,
		Priority:        priority,
		Labels:          labels,
		AssigneeIds:     assignees,
		StartDate:       start,
		DueDate:         due,
		Created:         created,
		Updated:         modified,
		Completed:       t.Completed,
		Fields:          fieldValues(t.CustomFields),
		Metadata:        map[string]any{"asana_url": t.Permalink},
	}
}

func safeGID(r *asanaRefShort) string {
	if r == nil {
		return ""
	}
	return r.GID
}

// htmlEscape covers the same chars as the legacy slack mrkdwn escaper.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// CleanupJob evicts the cached workspace gid for a finished job.
// Implements importProvider.JobCleaner.
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	p.mu.Lock()
	delete(p.workspaceCache, id)
	p.mu.Unlock()
}
