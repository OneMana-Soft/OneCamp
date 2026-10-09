// Package notion implements the Import Provider interface for Notion.
//
// Source: live API only ("api"). Notion's HTML/Markdown export ZIP is
// supported in a separate scaffold (export.go) but disabled until tested.
//
// Auth: Bearer <token>. Either a "Internal integration token" from
// notion.so/my-integrations or an OAuth2 access token. Stored encrypted
// via importModels.SaveToken.
//
// Rate limits: 3 requests/second per token. SleepLimiter every 333ms
// with burst 3 (~3 r/s sustained).
//
// Database selection:
//
//	The operator picks one or more "task databases" via
//	opts.databases (array of database ids). If absent we discover all
//	databases shared with the integration by POST /v1/search and pick
//	those whose properties contain Status + Assignee + Title (the
//	"task database" heuristic).
//
// Mapping:
//
//	Each database → OneCamp project.
//	Each row     → OneCamp task. Its Status property → status raw value.
//	Sub-pages (children of a row) → rendered into the task's description
//	so we don't lose content. Block trees use the renderer in blocks.go.
//	/v1/comments         → task comments.
//	block.file/image/pdf → attachments. URLs are short-lived presigned
//	S3 links; FetchAttachment refreshes via /v1/blocks/{id}.
package notion

import (
	"bytes"
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

const providerName = importModels.ProviderNotion
const apiBase = "https://api.notion.com/v1"

// notionVersion is the Notion-Version header. Pinned because Notion's
// breaking changes are versioned via this header.
const notionVersion = "2022-06-28"

const maxPages = 200

// Provider implements the importProvider.Provider interface for Notion.
type Provider struct {
	rl importProvider.Limiter

	// dbCache memoises which database ids the operator selected for an
	// import. Resolved once per job.
	mu      sync.Mutex
	dbCache map[uuid.UUID][]string
}

func New() *Provider {
	return &Provider{
		rl:      importProvider.NewSleepLimiter(333*time.Millisecond, 3),
		dbCache: make(map[uuid.UUID][]string, 4),
	}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTeams |
		importProvider.CapProjects |
		importProvider.CapTasks |
		importProvider.CapTaskComments |
		importProvider.CapAttachments
}

func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		"not started": "todo",
		"to-do":       "todo",
		"todo":        "todo",
		"in progress": "inProgress",
		"doing":       "inProgress",
		"in review":   "inReview",
		"review":      "inReview",
		"done":        "done",
		"complete":    "done",
		"completed":   "done",
		"cancelled":   "canceled",
		"archived":    "canceled",
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

// Validate checks /v1/users/me succeeds.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := p.getJSON(ctx, tok, "/users/me", &resp); err != nil {
		return fmt.Errorf("notion auth: %w", err)
	}
	if resp.ID == "" {
		return errors.New("notion /users/me returned empty id")
	}
	return nil
}

// Plan resolves the target databases and counts rows per database.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	dbIDs, err := p.resolveDatabases(ctx, j, opts)
	if err != nil {
		return nil, nil, err
	}
	if len(dbIDs) == 0 {
		return nil, nil, errors.New("notion: no task databases found; share at least one database with the integration or set opts.databases")
	}

	plan := &importProvider.Plan{
		ProjectCount: len(dbIDs),
	}

	// Users (all members of the workspace the integration sees).
	users, err := p.listUsers(ctx, tok)
	if err == nil {
		plan.UserCount = len(users)
	}

	statusSet := map[string]struct{}{}
	prioSet := map[string]struct{}{}
	for _, dbID := range dbIDs {
		// Database schema → status property options.
		props, err := p.fetchDatabaseProperties(ctx, tok, dbID)
		if err == nil {
			for _, opt := range props.statusOptions() {
				statusSet[strings.ToLower(opt)] = struct{}{}
			}
			for _, opt := range props.priorityOptions() {
				prioSet[strings.ToLower(opt)] = struct{}{}
			}
		}
		count, err := p.countDatabaseRows(ctx, tok, dbID, 5000)
		if err == nil {
			plan.TaskCount += count
		}
	}
	for s := range statusSet {
		plan.StatusValues = append(plan.StatusValues, s)
	}
	for s := range prioSet {
		plan.PriorityValues = append(plan.PriorityValues, s)
	}

	// Document the API limitation around inline-block comments. Notion
	// surfaces page-level comments via /v1/comments?block_id=<page_id>
	// but inline comments attached to individual blocks are not
	// returned by that call. We don't recursively walk every block to
	// fetch their per-block comments because the cost is unbounded for
	// large pages. Operators see this in the Plan UI.
	plan.Warnings = append(plan.Warnings,
		"Notion: only page-level comments are imported; inline-block discussions on individual paragraphs/lists are not surfaced by the public API")

	return plan, nil, nil
}

// IterUsers streams /v1/users.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("notion.IterUsers", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		users, err := p.listUsers(ctx, tok)
		if err != nil {
			errCh <- err
			return
		}
		importBots, _ := opts["import_bot_users"].(bool)
		for _, u := range users {
			isBot := u.Type == "bot"
			if isBot && !importBots {
				continue
			}
			email := ""
			if u.Person != nil {
				email = u.Person.Email
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    u.ID,
				DisplayName: u.Name,
				Email:       email,
				AvatarURL:   u.AvatarURL,
				IsBot:       isBot,
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams: Notion has workspaces, not teams. Empty stream.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam)
	errCh := make(chan error, 1)
	close(out)
	close(errCh)
	return out, errCh
}

// IterProjects emits one project per selected database.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 4)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("notion.IterProjects", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		dbIDs, err := p.resolveDatabases(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		// Members: every workspace user. Notion has no per-database
		// member list exposed via API. Permission filtering is then
		// the user's responsibility post-import.
		users, _ := p.listUsers(ctx, tok)
		members := make([]string, 0, len(users))
		for _, u := range users {
			if u.Type == "person" {
				members = append(members, u.ID)
			}
		}
		for _, dbID := range dbIDs {
			db, err := p.fetchDatabase(ctx, tok, dbID)
			if err != nil {
				continue
			}
			schema := schemaOf(db.Properties)
			created, _ := time.Parse(time.RFC3339, db.CreatedTime)
			name := databaseTitle(db)
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:    db.ID,
				Name:        name,
				Description: "",
				MemberIds:   members,
				Created:     created,
				Archived:    db.Archived,
				Fields:      schema.fieldList,
				Metadata:    map[string]any{"notion_url": db.URL},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject paginates POST /v1/databases/{id}/query.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("notion.IterTasksOfProject", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		// Cache the schema so we don't refetch per page.
		schema, err := p.fetchDatabaseProperties(ctx, tok, projectSourceId)
		if err != nil {
			errCh <- err
			return
		}
		startCursor := ""
		page := 0
		// Try a created_time sort first; Notion silently accepts it on
		// most databases but rejects with 400 on some legacy schemas.
		// Track whether we've fallen back to unsorted iteration so the
		// retry only happens once per job-stage.
		useSort := true
		for page < maxPages {
			page++
			body := map[string]any{
				"page_size": 100,
			}
			if useSort {
				body["sorts"] = []map[string]any{
					{"timestamp": "created_time", "direction": "ascending"},
				}
			}
			if startCursor != "" {
				body["start_cursor"] = startCursor
			}
			var resp notionQueryResp
			if err := p.postJSON(ctx, tok, "/databases/"+url.PathEscape(projectSourceId)+"/query", body, &resp); err != nil {
				if useSort && strings.Contains(err.Error(), "could not sort") {
					useSort = false
					page--
					continue
				}
				errCh <- err
				return
			}
			for _, row := range resp.Results {
				task, err := p.buildTaskFromPage(ctx, tok, &row, schema, projectSourceId)
				if err != nil {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- task:
				}
			}
			if !resp.HasMore || resp.NextCursor == "" {
				return
			}
			startCursor = resp.NextCursor
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask: Notion has no native subtasks. If a database has
// a "Sub-tasks" relation pointing back to itself, surface those rows.
// For now we emit nothing — Notion users typically nest content via
// child pages, which we already render into the description.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask)
	errCh := make(chan error, 1)
	close(out)
	close(errCh)
	return out, errCh
}

// IterCommentsOfTask paginates GET /v1/comments?block_id={page_id}.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("notion.IterCommentsOfTask", errCh)
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		startCursor := ""
		page := 0
		for page < maxPages {
			page++
			endpoint := "/comments?block_id=" + url.QueryEscape(taskSourceId) + "&page_size=100"
			if startCursor != "" {
				endpoint += "&start_cursor=" + url.QueryEscape(startCursor)
			}
			var resp notionCommentsResp
			if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
				errCh <- err
				return
			}
			for _, c := range resp.Results {
				created, _ := time.Parse(time.RFC3339, c.CreatedTime)
				body := renderRichText(c.RichText)
				author := ""
				if c.CreatedBy != nil {
					author = c.CreatedBy.ID
				}
				select {
				case <-ctx.Done():
					return
				case out <- importProvider.SourceComment{
					SourceID:       c.ID,
					TaskSourceID:   taskSourceId,
					Body:           body,
					AuthorSourceID: author,
					Created:        created,
				}:
				}
			}
			if !resp.HasMore || resp.NextCursor == "" {
				return
			}
			startCursor = resp.NextCursor
		}
	}()
	return out, errCh
}

// FetchAttachment refreshes the file URL by re-fetching its block
// (Notion file URLs are short-lived presigned S3 links). The block id
// is encoded in att.SourceID; we expect the format "<page_id>/<block_id>"
// or just "<block_id>" — both are accepted.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", 0, err
	}
	blockID := att.SourceID
	if i := strings.LastIndexByte(blockID, '/'); i >= 0 {
		blockID = blockID[i+1:]
	}
	var blk notionBlock
	if err := p.getJSON(ctx, tok, "/blocks/"+url.PathEscape(blockID), &blk); err != nil {
		return "", 0, err
	}
	url := ""
	switch blk.Type {
	case "image":
		if blk.Image != nil {
			url = blk.Image.fileURL()
		}
	case "file":
		if blk.File != nil {
			url = blk.File.fileURL()
		}
	case "pdf":
		if blk.PDF != nil {
			url = blk.PDF.fileURL()
		}
	}
	if url == "" {
		return "", 0, importProvider.ErrAttachmentGone
	}
	dl := att
	dl.URL = url
	dl.Headers = nil // presigned
	return importProvider.DefaultFetchAttachment(ctx, dl, dest)
}

// ─── Notion DTOs (subset we use) ─────────────────────────────────

type notionUser struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Person    *struct {
		Email string `json:"email"`
	} `json:"person"`
}

type notionDatabase struct {
	ID          string                  `json:"id"`
	URL         string                  `json:"url"`
	CreatedTime string                  `json:"created_time"`
	Archived    bool                    `json:"archived"`
	Title       []notionRichText        `json:"title"`
	Properties  map[string]notionDBProp `json:"properties"`
}

func databaseTitle(db *notionDatabase) string {
	t := renderRichTextPlain(db.Title)
	if t == "" {
		return "Imported Notion database"
	}
	return t
}

type notionDBProp struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status *struct {
		Options []notionOption `json:"options"`
	} `json:"status"`
	Select *struct {
		Options []notionOption `json:"options"`
	} `json:"select"`
	People      *struct{} `json:"people,omitempty"`
	Title       *struct{} `json:"title,omitempty"`
	Date        *struct{} `json:"date,omitempty"`
	MultiSelect *struct {
		Options []notionOption `json:"options"`
	} `json:"multi_select,omitempty"`
	Number *struct {
		Format string `json:"format"`
	} `json:"number,omitempty"`
}

// notionDBSchema bundles a database's properties with helpers that the
// IterTasksOfProject branch uses to drive task fields off the schema.
type notionDBSchema struct {
	props        map[string]notionDBProp
	titleProp    string
	statusProp   string
	priorityProp string
	assigneeProp string
	dueProp      string
	// The properties that are custom fields (fields.go).
	fieldList   []importProvider.SourceField
	fieldByName map[string]importProvider.SourceField
}

// statusOptions / priorityOptions return the configured options for the
// status / priority property.
func (s *notionDBSchema) statusOptions() []string {
	return propOptions(s.props, s.statusProp)
}
func (s *notionDBSchema) priorityOptions() []string {
	return propOptions(s.props, s.priorityProp)
}
func propOptions(props map[string]notionDBProp, name string) []string {
	if name == "" {
		return nil
	}
	p := props[name]
	if p.Type == "status" && p.Status != nil {
		out := make([]string, 0, len(p.Status.Options))
		for _, o := range p.Status.Options {
			out = append(out, o.Name)
		}
		return out
	}
	if p.Type == "select" && p.Select != nil {
		out := make([]string, 0, len(p.Select.Options))
		for _, o := range p.Select.Options {
			out = append(out, o.Name)
		}
		return out
	}
	return nil
}

type notionPage struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	Archived       bool   `json:"archived"`
	CreatedTime    string `json:"created_time"`
	LastEditedTime string `json:"last_edited_time"`
	CreatedBy      *struct {
		ID string `json:"id"`
	} `json:"created_by"`
	LastEditedBy *struct {
		ID string `json:"id"`
	} `json:"last_edited_by"`
	Properties map[string]notionPageProperty `json:"properties"`
}

type notionPageProperty struct {
	Type        string           `json:"type"`
	Title       []notionRichText `json:"title,omitempty"`
	RichText    []notionRichText `json:"rich_text,omitempty"`
	Status      *notionOption    `json:"status,omitempty"`
	Select      *notionOption    `json:"select,omitempty"`
	MultiSelect []notionOption   `json:"multi_select,omitempty"`
	People      []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"people,omitempty"`
	Date *struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"date,omitempty"`
	Checkbox bool     `json:"checkbox,omitempty"`
	Number   *float64 `json:"number,omitempty"`
	URL      *string  `json:"url,omitempty"`
	Email    *string  `json:"email,omitempty"`
	Phone    *string  `json:"phone_number,omitempty"`
}

type notionQueryResp struct {
	Results    []notionPage `json:"results"`
	HasMore    bool         `json:"has_more"`
	NextCursor string       `json:"next_cursor"`
}

type notionRichText struct {
	Type        string `json:"type"`
	PlainText   string `json:"plain_text"`
	Annotations struct {
		Bold          bool   `json:"bold"`
		Italic        bool   `json:"italic"`
		Strikethrough bool   `json:"strikethrough"`
		Underline     bool   `json:"underline"`
		Code          bool   `json:"code"`
		Color         string `json:"color"`
	} `json:"annotations"`
	Text *struct {
		Content string `json:"content"`
		Link    *struct {
			URL string `json:"url"`
		} `json:"link"`
	} `json:"text,omitempty"`
	Mention *struct {
		Type string `json:"type"`
	} `json:"mention,omitempty"`
}

// notionBlock covers the union of block types we render. Unknown types
// fall through to the default branch in renderOneBlock.
type notionBlock struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	HasChildren bool          `json:"has_children"`
	Children    []notionBlock `json:"-"` // populated by recursive fetch

	Paragraph        *notionRichTextBox `json:"paragraph,omitempty"`
	Heading1         *notionRichTextBox `json:"heading_1,omitempty"`
	Heading2         *notionRichTextBox `json:"heading_2,omitempty"`
	Heading3         *notionRichTextBox `json:"heading_3,omitempty"`
	BulletedListItem *notionRichTextBox `json:"bulleted_list_item,omitempty"`
	NumberedListItem *notionRichTextBox `json:"numbered_list_item,omitempty"`
	ToDo             *notionToDoBox     `json:"to_do,omitempty"`
	Code             *notionCodeBox     `json:"code,omitempty"`
	Quote            *notionRichTextBox `json:"quote,omitempty"`
	Callout          *notionRichTextBox `json:"callout,omitempty"`
	Image            *notionFileBox     `json:"image,omitempty"`
	File             *notionFileBox     `json:"file,omitempty"`
	PDF              *notionFileBox     `json:"pdf,omitempty"`
	Bookmark         *notionURLBox      `json:"bookmark,omitempty"`
	Equation         *struct {
		Expression string `json:"expression"`
	} `json:"equation,omitempty"`
	TableRow *struct {
		Cells [][]notionRichText `json:"cells"`
	} `json:"table_row,omitempty"`
}

type notionRichTextBox struct {
	RichText []notionRichText `json:"rich_text"`
}
type notionToDoBox struct {
	RichText []notionRichText `json:"rich_text"`
	Checked  bool             `json:"checked"`
}
type notionCodeBox struct {
	RichText []notionRichText `json:"rich_text"`
	Language string           `json:"language"`
}
type notionFileBox struct {
	Caption  []notionRichText `json:"caption"`
	Type     string           `json:"type"`
	External *struct {
		URL string `json:"url"`
	} `json:"external,omitempty"`
	File *struct {
		URL        string `json:"url"`
		ExpiryTime string `json:"expiry_time"`
	} `json:"file,omitempty"`
	Name string `json:"name,omitempty"`
}

// fileURL returns whichever of file/external is present.
func (f *notionFileBox) fileURL() string {
	if f == nil {
		return ""
	}
	if f.External != nil && f.External.URL != "" {
		return f.External.URL
	}
	if f.File != nil && f.File.URL != "" {
		return f.File.URL
	}
	return ""
}

type notionURLBox struct {
	URL     string           `json:"url"`
	Caption []notionRichText `json:"caption"`
}

type notionComment struct {
	ID          string `json:"id"`
	CreatedTime string `json:"created_time"`
	CreatedBy   *struct {
		ID string `json:"id"`
	} `json:"created_by"`
	RichText []notionRichText `json:"rich_text"`
}
type notionCommentsResp struct {
	Results    []notionComment `json:"results"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor"`
}

// ─── HTTP helpers ─────────────────────────────────────────────────

func (p *Provider) getJSON(ctx context.Context, tok, path string, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Accept", "application/json")
	return p.do(ctx, req, out)
}

func (p *Provider) postJSON(ctx context.Context, tok, path string, body, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return p.do(ctx, req, out)
}

func (p *Provider) do(ctx context.Context, req *http.Request, out any) error {
	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 10 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return &importProvider.ErrRateLimited{RetryAfter: retryAfter, Reason: "Notion 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &importProvider.TokenRejected{Msg: fmt.Sprintf("notion auth failed (HTTP %d); reconnect token", resp.StatusCode)}
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notion HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return json.NewDecoder(resp.Body).Decode(out)
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
		return "", errors.New("empty notion token")
	}
	return t.AccessToken, nil
}

// ─── Helpers for the iteration loops ─────────────────────────────

func (p *Provider) listUsers(ctx context.Context, tok string) ([]notionUser, error) {
	out := []notionUser{}
	startCursor := ""
	page := 0
	for page < maxPages {
		page++
		endpoint := "/users?page_size=100"
		if startCursor != "" {
			endpoint += "&start_cursor=" + url.QueryEscape(startCursor)
		}
		var resp struct {
			Results    []notionUser `json:"results"`
			HasMore    bool         `json:"has_more"`
			NextCursor string       `json:"next_cursor"`
		}
		if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Results...)
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		startCursor = resp.NextCursor
	}
	return out, nil
}

func (p *Provider) fetchDatabase(ctx context.Context, tok, dbID string) (*notionDatabase, error) {
	var db notionDatabase
	if err := p.getJSON(ctx, tok, "/databases/"+url.PathEscape(dbID), &db); err != nil {
		return nil, err
	}
	return &db, nil
}

func (p *Provider) fetchDatabaseProperties(ctx context.Context, tok, dbID string) (*notionDBSchema, error) {
	db, err := p.fetchDatabase(ctx, tok, dbID)
	if err != nil {
		return nil, err
	}
	return schemaOf(db.Properties), nil
}

// schemaOf picks which properties are the task's own: its title, status,
// priority, assignee and due date. The rest may be custom fields.
func schemaOf(props map[string]notionDBProp) *notionDBSchema {
	s := &notionDBSchema{props: props}
	for n, prop := range props {
		switch prop.Type {
		case "title":
			s.titleProp = n
		case "status":
			s.statusProp = n
		case "select":
			lower := strings.ToLower(n)
			if lower == "priority" || lower == "p" {
				s.priorityProp = n
			} else if s.statusProp == "" && (lower == "status" || lower == "state") {
				s.statusProp = n
			}
		case "people":
			lower := strings.ToLower(n)
			if lower == "assignee" || lower == "assigned to" || lower == "owner" {
				s.assigneeProp = n
			}
		case "date":
			lower := strings.ToLower(n)
			if lower == "due" || lower == "due date" || lower == "deadline" {
				s.dueProp = n
			}
		}
	}
	s.fieldList, s.fieldByName = s.buildFields()
	return s
}

func (p *Provider) countDatabaseRows(ctx context.Context, tok, dbID string, cap int) (int, error) {
	startCursor := ""
	count := 0
	page := 0
	for page < maxPages {
		page++
		body := map[string]any{"page_size": 100}
		if startCursor != "" {
			body["start_cursor"] = startCursor
		}
		var resp struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}
		if err := p.postJSON(ctx, tok, "/databases/"+url.PathEscape(dbID)+"/query", body, &resp); err != nil {
			return count, err
		}
		count += len(resp.Results)
		if count >= cap {
			return count, nil
		}
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		startCursor = resp.NextCursor
	}
	return count, nil
}

// resolveDatabases returns the list of database IDs to import.
//  1. If opts.databases is a non-empty []string, use it verbatim.
//  2. Otherwise call POST /v1/search?filter.value=database to enumerate
//     databases shared with the integration, and pick those that look
//     like task databases (have title + status/select).
func (p *Provider) resolveDatabases(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) ([]string, error) {
	p.mu.Lock()
	if cached, ok := p.dbCache[j.Id]; ok {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	var ids []string
	if v, ok := opts["databases"]; ok {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok && s != "" {
					ids = append(ids, s)
				}
			}
		case []string:
			ids = append(ids, t...)
		}
	}
	if len(ids) > 0 {
		p.mu.Lock()
		p.dbCache[j.Id] = ids
		p.mu.Unlock()
		return ids, nil
	}

	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"filter":    map[string]any{"value": "database", "property": "object"},
		"page_size": 100,
	}
	var resp struct {
		Results    []notionDatabase `json:"results"`
		HasMore    bool             `json:"has_more"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := p.postJSON(ctx, tok, "/search", body, &resp); err != nil {
		return nil, err
	}
	for _, db := range resp.Results {
		if looksLikeTaskDatabase(db.Properties) {
			ids = append(ids, db.ID)
		}
	}
	p.mu.Lock()
	p.dbCache[j.Id] = ids
	p.mu.Unlock()
	return ids, nil
}

// looksLikeTaskDatabase matches any database with a Title and at least
// one of (Status property | Select named "Status" | "Done"-shaped Checkbox).
func looksLikeTaskDatabase(props map[string]notionDBProp) bool {
	hasTitle := false
	hasStatusish := false
	for n, p := range props {
		if p.Type == "title" {
			hasTitle = true
		}
		lower := strings.ToLower(n)
		if p.Type == "status" {
			hasStatusish = true
		}
		if p.Type == "select" && (lower == "status" || lower == "state") {
			hasStatusish = true
		}
	}
	return hasTitle && hasStatusish
}

// buildTaskFromPage builds a SourceTask from a Notion page row.
// Description is rendered from the page's child blocks (depth-first
// fetch up to 1 child level deep — Notion's API requires per-block
// child fetches, which we do for the top-level children only).
func (p *Provider) buildTaskFromPage(ctx context.Context, tok string, row *notionPage, schema *notionDBSchema, projectID string) (importProvider.SourceTask, error) {
	created, _ := time.Parse(time.RFC3339, row.CreatedTime)
	updated, _ := time.Parse(time.RFC3339, row.LastEditedTime)

	name := ""
	if schema.titleProp != "" {
		if pp, ok := row.Properties[schema.titleProp]; ok {
			name = renderRichTextPlain(pp.Title)
		}
	}
	if name == "" {
		name = "Untitled"
	}

	status := ""
	if schema.statusProp != "" {
		if pp, ok := row.Properties[schema.statusProp]; ok {
			if pp.Status != nil {
				status = pp.Status.Name
			} else if pp.Select != nil {
				status = pp.Select.Name
			}
		}
	}
	priority := ""
	if schema.priorityProp != "" {
		if pp, ok := row.Properties[schema.priorityProp]; ok && pp.Select != nil {
			priority = pp.Select.Name
		}
	}
	assignees := []string{}
	if schema.assigneeProp != "" {
		if pp, ok := row.Properties[schema.assigneeProp]; ok {
			for _, p := range pp.People {
				assignees = append(assignees, p.ID)
			}
		}
	}
	var due *time.Time
	if schema.dueProp != "" {
		if pp, ok := row.Properties[schema.dueProp]; ok && pp.Date != nil && pp.Date.Start != "" {
			if t, err := time.Parse(time.RFC3339, pp.Date.Start); err == nil {
				due = &t
			} else if t, err := time.Parse("2006-01-02", pp.Date.Start); err == nil {
				due = &t
			}
		}
	}
	// A Tags or Labels multi-select is the task's tags; other
	// multi-selects are custom fields.
	labels := []string{}
	for n, prop := range row.Properties {
		if prop.Type == "multi_select" && isTagsProp(n) {
			for _, ms := range prop.MultiSelect {
				labels = append(labels, ms.Name)
			}
		}
	}

	// Description: fetch the page's top-level children and render them.
	body := ""
	children, err := p.fetchBlockChildren(ctx, tok, row.ID, 1)
	if err == nil {
		body = renderBlocks(children)
	}

	atts := []importProvider.SourceAttachment{}
	for _, blk := range children {
		switch blk.Type {
		case "image":
			if blk.Image != nil && blk.Image.fileURL() != "" {
				atts = append(atts, importProvider.SourceAttachment{
					SourceID: blk.ID, // resolved fresh in FetchAttachment
					URL:      blk.Image.fileURL(),
					Mime:     "image/*",
					Parent:   importProvider.SourceRef{Kind: "task", SourceID: row.ID},
				})
			}
		case "file":
			if blk.File != nil && blk.File.fileURL() != "" {
				atts = append(atts, importProvider.SourceAttachment{
					SourceID: blk.ID,
					URL:      blk.File.fileURL(),
					Name:     blk.File.Name,
					Parent:   importProvider.SourceRef{Kind: "task", SourceID: row.ID},
				})
			}
		case "pdf":
			if blk.PDF != nil && blk.PDF.fileURL() != "" {
				atts = append(atts, importProvider.SourceAttachment{
					SourceID: blk.ID,
					URL:      blk.PDF.fileURL(),
					Name:     blk.PDF.Name,
					Mime:     "application/pdf",
					Parent:   importProvider.SourceRef{Kind: "task", SourceID: row.ID},
				})
			}
		}
	}

	createdBy := ""
	if row.CreatedBy != nil {
		createdBy = row.CreatedBy.ID
	}

	return importProvider.SourceTask{
		SourceID:        row.ID,
		ProjectSourceID: projectID,
		Name:            name,
		Description:     body,
		Status:          status,
		Priority:        priority,
		Labels:          labels,
		AssigneeIds:     assignees,
		CreatedBy:       createdBy,
		DueDate:         due,
		Created:         created,
		Updated:         updated,
		Completed:       isDoneStatus(status),
		AttachmentRefs:  atts,
		Fields:          fieldValues(row.Properties, schema.fieldByName),
		Metadata:        map[string]any{"notion_url": row.URL},
	}, nil
}

// fetchBlockChildren fetches /v1/blocks/{id}/children (paginated).
// Recurses up to depth (typically 1 — the children of children would
// blow up the description size for no marginal benefit).
func (p *Provider) fetchBlockChildren(ctx context.Context, tok, parentID string, depth int) ([]notionBlock, error) {
	out := []notionBlock{}
	startCursor := ""
	page := 0
	for page < maxPages {
		page++
		endpoint := "/blocks/" + url.PathEscape(parentID) + "/children?page_size=100"
		if startCursor != "" {
			endpoint += "&start_cursor=" + url.QueryEscape(startCursor)
		}
		var resp struct {
			Results    []notionBlock `json:"results"`
			HasMore    bool          `json:"has_more"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
			return out, err
		}
		out = append(out, resp.Results...)
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		startCursor = resp.NextCursor
	}
	if depth > 0 {
		for i := range out {
			if !out[i].HasChildren {
				continue
			}
			children, err := p.fetchBlockChildren(ctx, tok, out[i].ID, depth-1)
			if err != nil {
				continue
			}
			out[i].Children = children
		}
	}
	return out, nil
}

func isDoneStatus(s string) bool {
	switch strings.ToLower(s) {
	case "done", "complete", "completed", "closed", "resolved":
		return true
	}
	return false
}

// CleanupJob evicts the cached database list for a finished job.
// Implements importProvider.JobCleaner.
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	p.mu.Lock()
	delete(p.dbCache, id)
	p.mu.Unlock()
}
