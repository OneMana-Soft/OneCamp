// Package jira implements the Import Provider interface for Jira Cloud.
//
// Source: live API only ("api"). The Jira Cloud Backup XML export shape
// is supported via a separate parser file (parser.go) but disabled by
// default until tested on real data.
//
// Auth: Basic <base64(email:api_token)>. Atlassian's recommended PAT
// flow uses the email + API token combo; we pack both into the connect
// payload's metadata.email and access_token. OAuth 2.0 (3LO) is also
// supported by passing access_token only and the Atlassian site URL in
// metadata.site_url + metadata.cloud_id.
//
// Rate limits: Atlassian uses a per-instance budget (~10 r/s/host) plus
// per-user concurrency. We use SleepLimiter at 100 r/s with burst 10
// — well under the platform limit.
//
// Pagination: every list endpoint uses startAt + maxResults. We follow
// up to a safety cap of 200 pages.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
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

const providerName = importModels.ProviderJira

// maxPages caps pagination at 200 × 100 = 20,000 items per call. Real
// Jira projects rarely exceed 10k issues; the cap exists to bound a
// runaway loop.
const maxPages = 200

// Provider implements the importProvider.Provider interface for Jira.
type Provider struct {
	rl importProvider.Limiter

	// siteCache maps job id → resolved Jira API base URL. Caches the
	// site URL so each Iter* doesn't re-resolve from token metadata.
	mu        sync.Mutex
	siteCache map[uuid.UUID]string
	// fieldsCache maps job id → the site's custom fields (fields.go).
	fieldsCache map[uuid.UUID]siteFields
}

func New() *Provider {
	return &Provider{
		rl:          importProvider.NewSleepLimiter(100*time.Millisecond, 10),
		siteCache:   make(map[uuid.UUID]string, 4),
		fieldsCache: make(map[uuid.UUID]siteFields, 4),
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
		"to do":                    "backlog",
		"open":                     "backlog",
		"backlog":                  "backlog",
		"selected for development": "todo",
		"todo":                     "todo",
		"in progress":              "inProgress",
		"in development":           "inProgress",
		"in review":                "inReview",
		"code review":              "inReview",
		"qa":                       "inReview",
		"done":                     "done",
		"closed":                   "done",
		"resolved":                 "done",
		"cancelled":                "canceled",
		"won't do":                 "canceled",
		"won't fix":                "canceled",
	}
}

func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"highest": "high",
		"high":    "high",
		"medium":  "medium",
		"low":     "low",
		"lowest":  "low",
	}
}

// Validate calls /rest/api/3/myself.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadAuth(ctx, j)
	if err != nil {
		return err
	}
	site, err := p.resolveSite(ctx, j)
	if err != nil {
		return err
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := p.getJSON(ctx, tok, site+"/rest/api/3/myself", &me); err != nil {
		return fmt.Errorf("jira auth: %w", err)
	}
	if me.AccountID == "" {
		return errors.New("jira /myself returned empty accountId")
	}
	return nil
}

// Plan lists projects and runs a JQL search per project to count issues.
// Status values are aggregated from project-specific status sets so the
// operator's mapping UI shows the actual statuses configured for this
// instance (Jira lets each project configure its own workflow).
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	tok, err := p.loadAuth(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	site, err := p.resolveSite(ctx, j)
	if err != nil {
		return nil, nil, err
	}

	projects, err := p.listProjects(ctx, tok, site)
	if err != nil {
		return nil, nil, fmt.Errorf("jira projects: %w", err)
	}

	plan := &importProvider.Plan{
		ProjectCount: len(projects),
	}

	users, err := p.listUsers(ctx, tok, site)
	if err == nil {
		plan.UserCount = len(users)
	}

	statusSet := map[string]struct{}{}
	prioritySet := map[string]struct{}{}

	for _, pr := range projects {
		// Issue count via JQL search. We only need .total from the response.
		count, err := p.countIssues(ctx, tok, site, pr.Key)
		if err == nil {
			plan.TaskCount += count
		}
		// Status set per project.
		statuses, _ := p.listProjectStatuses(ctx, tok, site, pr.Key)
		for _, s := range statuses {
			n := strings.ToLower(strings.TrimSpace(s))
			if n != "" {
				statusSet[n] = struct{}{}
			}
		}
	}
	// Priorities are global to the instance.
	priorities, _ := p.listPriorities(ctx, tok, site)
	for _, pr := range priorities {
		n := strings.ToLower(strings.TrimSpace(pr))
		if n != "" {
			prioritySet[n] = struct{}{}
		}
	}

	for s := range statusSet {
		plan.StatusValues = append(plan.StatusValues, s)
	}
	for s := range prioritySet {
		plan.PriorityValues = append(plan.PriorityValues, s)
	}

	return plan, nil, nil
}

// IterUsers streams active workspace users via /rest/api/3/users/search.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterUsers", errCh)
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		users, err := p.listUsers(ctx, tok, site)
		if err != nil {
			errCh <- err
			return
		}
		for _, u := range users {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    u.AccountID,
				DisplayName: u.DisplayName,
				Email:       u.EmailAddress,
				AvatarURL:   helpers.FirstNonEmpty(u.AvatarURLs.URL48x48, u.AvatarURLs.URL32x32),
				IsBot:       u.AccountType == "app",
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits one synthetic team per project category when
// opts.group_by == "project_category", otherwise nothing (orchestrator
// falls back to the default team).
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam, 4)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterTeams", errCh)
		groupBy, _ := opts["group_by"].(string)
		if groupBy != "project_category" {
			return
		}
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		cats, err := p.listProjectCategories(ctx, tok, site)
		if err != nil {
			errCh <- err
			return
		}
		for _, c := range cats {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceTeam{
				SourceID:    c.ID,
				Name:        c.Name,
				Description: c.Description,
			}:
			}
		}
	}()
	return out, errCh
}

// IterProjects streams Jira projects.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterProjects", errCh)
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		projects, err := p.listProjects(ctx, tok, site)
		if err != nil {
			errCh <- err
			return
		}
		groupBy, _ := opts["group_by"].(string)
		for _, pr := range projects {
			team := ""
			if groupBy == "project_category" && pr.Category != nil {
				team = pr.Category.ID
			}
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:     pr.Key, // we use the human-friendly key; gid lookups still work via /project/{key}
				Name:         pr.Name,
				Description:  pr.Description,
				TeamSourceID: team,
				CreatedBy:    safeAccountID(pr.Lead),
				Metadata:     map[string]any{"jira_key": pr.Key, "jira_url": site + "/browse/" + pr.Key},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject paginates issues via JQL search ordered by created
// asc. We use `expand=renderedFields` to get description as HTML.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterTasksOfProject", errCh)
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		fields, err := p.customFields(ctx, j, tok, site)
		if err != nil {
			errCh <- err
			return
		}
		// Every navigable field, custom ones included: a site's custom fields
		// are too many to name one by one in a URL.
		asked := append([]string{"*navigable"}, strings.Split(issueFields, ",")...)
		jql := fmt.Sprintf(`project = "%s" ORDER BY created ASC`, projectSourceId)
		err = p.searchIssues(ctx, tok, site, jql, asked, func(iss *jiraIssue) bool {
			if iss.Fields.IssueType.Subtask {
				// Skip subtasks here; they're emitted by IterSubtasksOfTask.
				return true
			}
			select {
			case <-ctx.Done():
				return false
			case out <- buildSourceTask(iss, projectSourceId, site, fields):
			}
			// If the issue has subtasks, schedule a subtask chunk (once: the
			// search meets each issue once).
			if len(iss.Fields.Subtasks) > 0 {
				select {
				case <-ctx.Done():
					return false
				case out <- importProvider.SourceTask{SourceID: "__sub__:" + iss.Key, ParentTaskID: iss.Key}:
				}
			}
			return true
		})
		if err != nil {
			errCh <- err
		}
	}()
	return out, errCh
}

// searchIssues runs a JQL search (GET /rest/api/3/search/jql), calling each
// for every issue once, until the last page or until each says to stop.
// Pages follow nextPageToken; a page with no issue not seen before is the
// end too, since some sites go on handing out tokens past the last page.
func (p *Provider) searchIssues(ctx context.Context, tok, site, jql string, fields []string, each func(*jiraIssue) bool) error {
	seen := map[string]bool{}
	token := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("jql", jql)
		q.Set("maxResults", "100")
		q.Set("fields", strings.Join(fields, ","))
		q.Set("expand", "renderedFields")
		if token != "" {
			q.Set("nextPageToken", token)
		}
		var resp jiraSearchResp
		if err := p.getJSON(ctx, tok, site+"/rest/api/3/search/jql?"+q.Encode(), &resp); err != nil {
			return err
		}
		fresh := 0
		for i := range resp.Issues {
			iss := &resp.Issues[i]
			if seen[iss.Key] {
				continue
			}
			seen[iss.Key] = true
			fresh++
			if !each(iss) {
				return nil
			}
		}
		if resp.IsLast || resp.NextPageToken == "" || resp.NextPageToken == token || fresh == 0 {
			return nil
		}
		token = resp.NextPageToken
	}
	return nil
}

// IterSubtasksOfTask fetches the parent issue with subtasks expanded.
// Each subtask entry from the parent's `subtasks` field is itself
// fetched in full (the parent payload only carries id/key/summary).
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterSubtasksOfTask", errCh)
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		fields, err := p.customFields(ctx, j, tok, site)
		if err != nil {
			errCh <- err
			return
		}
		// Pull parent with subtasks.
		var parent jiraIssue
		if err := p.getJSON(ctx, tok,
			site+"/rest/api/3/issue/"+url.PathEscape(taskSourceId)+"?fields=subtasks", &parent); err != nil {
			errCh <- err
			return
		}
		for _, st := range parent.Fields.Subtasks {
			// Refetch full subtask shape for the source task.
			var iss jiraIssue
			if err := p.getJSON(ctx, tok,
				site+"/rest/api/3/issue/"+url.PathEscape(st.Key)+"?expand=renderedFields", &iss); err != nil {
				continue // tolerate per-subtask failures
			}
			task := buildSourceTask(&iss, projectKeyFromIssue(&iss), site, fields)
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

// IterCommentsOfTask paginates /issue/{key}/comment.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("jira.IterCommentsOfTask", errCh)
		tok, err := p.loadAuth(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		site, err := p.resolveSite(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		startAt := 0
		page := 0
		for page < maxPages {
			page++
			endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s/comment?startAt=%d&maxResults=100&expand=renderedBody",
				site, url.PathEscape(taskSourceId), startAt)
			var resp jiraCommentsResp
			if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
				errCh <- err
				return
			}
			for _, c := range resp.Comments {
				body := c.RenderedBody
				if body == "" {
					body = htmlEscape(extractAtlassianText(c.Body))
				}
				created := parseJiraTime(c.Created)
				select {
				case <-ctx.Done():
					return
				case out <- importProvider.SourceComment{
					SourceID:       c.ID,
					TaskSourceID:   taskSourceId,
					Body:           body,
					AuthorSourceID: safeAccountID(c.Author),
					Created:        created,
				}:
				}
			}
			startAt += len(resp.Comments)
			if len(resp.Comments) == 0 || startAt >= resp.Total {
				return
			}
		}
	}()
	return out, errCh
}

// FetchAttachment streams /rest/api/3/attachment/content/{id}.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.loadAuth(ctx, j)
	if err != nil {
		return "", 0, err
	}
	site, err := p.resolveSite(ctx, j)
	if err != nil {
		return "", 0, err
	}
	// Either att.URL is the full /content URL (preferred) or we derive it.
	dlURL := att.URL
	if dlURL == "" {
		dlURL = site + "/rest/api/3/attachment/content/" + url.PathEscape(att.SourceID)
	}
	dl := att
	dl.URL = dlURL
	dl.Headers = map[string]string{"Authorization": "Basic " + tok}
	return importProvider.DefaultFetchAttachment(ctx, dl, dest)
}

// ─── Jira DTOs (subset we use) ───────────────────────────────────

type jiraUser struct {
	AccountID    string `json:"accountId"`
	AccountType  string `json:"accountType"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	AvatarURLs   struct {
		URL48x48 string `json:"48x48"`
		URL32x32 string `json:"32x32"`
	} `json:"avatarUrls"`
}

type jiraProjectCategory struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type jiraProject struct {
	ID          string               `json:"id"`
	Key         string               `json:"key"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Category    *jiraProjectCategory `json:"projectCategory"`
	Lead        *jiraUser            `json:"lead"`
}

type jiraIssue struct {
	ID             string              `json:"id"`
	Key            string              `json:"key"`
	Fields         jiraIssueFields     `json:"fields"`
	RenderedFields *jiraRenderedFields `json:"renderedFields"`
	// Custom holds the issue's customfield_NNNNN values that are set.
	Custom map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON reads an issue, keeping its custom fields' raw values,
// whose shapes depend on each field's kind (fields.go).
func (i *jiraIssue) UnmarshalJSON(b []byte) error {
	type plain jiraIssue
	if err := json.Unmarshal(b, (*plain)(i)); err != nil {
		return err
	}
	var raw struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	i.Custom = nil
	for k, v := range raw.Fields {
		if strings.HasPrefix(k, "customfield_") && len(v) > 0 && string(v) != "null" {
			if i.Custom == nil {
				i.Custom = map[string]json.RawMessage{}
			}
			i.Custom[k] = v
		}
	}
	return nil
}

type jiraIssueFields struct {
	Summary     string           `json:"summary"`
	Description any              `json:"description"` // ADF JSON object
	Status      jiraNamed        `json:"status"`
	Priority    *jiraNamed       `json:"priority"`
	IssueType   jiraIssueType    `json:"issuetype"`
	Assignee    *jiraUser        `json:"assignee"`
	Reporter    *jiraUser        `json:"reporter"`
	Creator     *jiraUser        `json:"creator"`
	Labels      []string         `json:"labels"`
	DueDate     string           `json:"duedate"`
	Created     string           `json:"created"`
	Updated     string           `json:"updated"`
	Parent      *jiraIssueRef    `json:"parent"`
	Subtasks    []jiraIssueRef   `json:"subtasks"`
	Attachment  []jiraAttachment `json:"attachment"`
	Project     *jiraProjectRef  `json:"project"`
}

type jiraRenderedFields struct {
	Description string `json:"description"`
}

type jiraNamed struct {
	Name string `json:"name"`
}
type jiraIssueType struct {
	Name    string `json:"name"`
	Subtask bool   `json:"subtask"`
}
type jiraIssueRef struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}
type jiraProjectRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type jiraAttachment struct {
	ID       string    `json:"id"`
	Filename string    `json:"filename"`
	MimeType string    `json:"mimeType"`
	Size     int64     `json:"size"`
	Content  string    `json:"content"` // direct download URL with embedded auth
	Author   *jiraUser `json:"author"`
	Created  string    `json:"created"`
}

// jiraSearchResp is a page of GET /rest/api/3/search/jql, which pages by
// token: there's no total, and nextPageToken is empty on the last page.
type jiraSearchResp struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken string      `json:"nextPageToken"`
	IsLast        bool        `json:"isLast"`
}

type jiraComment struct {
	ID           string    `json:"id"`
	Author       *jiraUser `json:"author"`
	Body         any       `json:"body"`         // ADF JSON
	RenderedBody string    `json:"renderedBody"` // HTML (with expand=renderedBody)
	Created      string    `json:"created"`
}

type jiraCommentsResp struct {
	StartAt    int           `json:"startAt"`
	MaxResults int           `json:"maxResults"`
	Total      int           `json:"total"`
	Comments   []jiraComment `json:"comments"`
}

// ─── HTTP helpers ─────────────────────────────────────────────────

// getJSON does GET <url> with the appropriate auth header. Honours 429.
// statusError is Jira answering with an error status.
type statusError struct {
	Code int
	Msg  string
}

func (e *statusError) Error() string { return e.Msg }

func (p *Provider) getJSON(ctx context.Context, tok, urlStr string, out any) error {
	return p.call(ctx, tok, http.MethodGet, urlStr, nil, out)
}

func (p *Provider) postJSON(ctx context.Context, tok, urlStr string, body, out any) error {
	return p.call(ctx, tok, http.MethodPost, urlStr, body, out)
}

func (p *Provider) call(ctx context.Context, tok, method, urlStr string, body, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// tok may be Basic <b64> or Bearer <pat>, we just prepend "Basic " if not already prefixed.
	if strings.HasPrefix(tok, "Bearer ") || strings.HasPrefix(tok, "Basic ") {
		req.Header.Set("Authorization", tok)
	} else {
		req.Header.Set("Authorization", "Basic "+tok)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
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
		return &importProvider.ErrRateLimited{RetryAfter: retryAfter, Reason: "Jira 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &statusError{Code: resp.StatusCode, Msg: fmt.Sprintf("jira auth failed (HTTP %d); reconnect token", resp.StatusCode)}
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(resp.Body)
		return &statusError{Code: resp.StatusCode, Msg: fmt.Sprintf("jira HTTP %d: %s", resp.StatusCode, string(raw))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *Provider) listUsers(ctx context.Context, tok, site string) ([]jiraUser, error) {
	out := []jiraUser{}
	startAt := 0
	page := 0
	for page < maxPages {
		page++
		endpoint := fmt.Sprintf("%s/rest/api/3/users/search?startAt=%d&maxResults=100", site, startAt)
		var users []jiraUser
		if err := p.getJSON(ctx, tok, endpoint, &users); err != nil {
			return nil, err
		}
		// Filter to atlassian (real) users.
		for _, u := range users {
			if u.AccountType == "atlassian" || u.AccountType == "" {
				out = append(out, u)
			}
		}
		if len(users) < 100 {
			break
		}
		startAt += len(users)
	}
	return out, nil
}

func (p *Provider) listProjects(ctx context.Context, tok, site string) ([]jiraProject, error) {
	out := []jiraProject{}
	startAt := 0
	page := 0
	for page < maxPages {
		page++
		endpoint := fmt.Sprintf("%s/rest/api/3/project/search?startAt=%d&maxResults=100&expand=description,lead,projectCategory", site, startAt)
		var resp struct {
			IsLast bool          `json:"isLast"`
			Values []jiraProject `json:"values"`
			Total  int           `json:"total"`
		}
		if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Values...)
		if resp.IsLast || len(resp.Values) == 0 {
			break
		}
		startAt += len(resp.Values)
	}
	return out, nil
}

func (p *Provider) listProjectStatuses(ctx context.Context, tok, site, projectKey string) ([]string, error) {
	endpoint := fmt.Sprintf("%s/rest/api/3/project/%s/statuses", site, url.PathEscape(projectKey))
	var resp []struct {
		Name     string `json:"name"`
		Statuses []struct {
			Name string `json:"name"`
		} `json:"statuses"`
	}
	if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
		return nil, err
	}
	out := []string{}
	seen := map[string]struct{}{}
	for _, t := range resp {
		for _, s := range t.Statuses {
			n := strings.TrimSpace(s.Name)
			if n == "" {
				continue
			}
			if _, dup := seen[strings.ToLower(n)]; dup {
				continue
			}
			seen[strings.ToLower(n)] = struct{}{}
			out = append(out, n)
		}
	}
	return out, nil
}

func (p *Provider) listPriorities(ctx context.Context, tok, site string) ([]string, error) {
	endpoint := site + "/rest/api/3/priority"
	var resp []jiraNamed
	if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp))
	for _, p := range resp {
		out = append(out, p.Name)
	}
	return out, nil
}

func (p *Provider) listProjectCategories(ctx context.Context, tok, site string) ([]jiraProjectCategory, error) {
	endpoint := site + "/rest/api/3/projectCategory"
	var resp []jiraProjectCategory
	if err := p.getJSON(ctx, tok, endpoint, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// countIssues asks Jira how many issues a project has. The search that
// used to say (GET /rest/api/3/search) is gone from Jira Cloud; this is
// its counting replacement.
func (p *Provider) countIssues(ctx context.Context, tok, site, projectKey string) (int, error) {
	var resp struct {
		Count int `json:"count"`
	}
	body := map[string]string{"jql": fmt.Sprintf(`project = "%s"`, projectKey)}
	if err := p.postJSON(ctx, tok, site+"/rest/api/3/search/approximate-count", body, &resp); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// ─── Auth + site resolution ──────────────────────────────────────

// loadAuth returns the value to put in the Authorization header.
// For email+API-token (the typical setup), the connect payload supplies
// metadata.email + access_token; we base64-encode "email:token".
// For OAuth2 (3LO), connect supplies access_token only with metadata.bearer="true".
func (p *Provider) loadAuth(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty jira token")
	}
	email := ""
	bearer := false
	if len(t.Metadata) > 0 {
		var md struct {
			Email  string `json:"email"`
			Bearer string `json:"bearer"`
		}
		_ = json.Unmarshal(t.Metadata, &md)
		email = md.Email
		bearer = md.Bearer == "true"
	}
	if bearer {
		// OAuth 2.0 (3LO) path: the access token expires in ~1 hour.
		// Route through the shared refresher so a long historical
		// import doesn't 401 mid-run. Falls back to the stored token
		// when JIRA_OAUTH_CLIENT_ID/SECRET aren't configured (PAT-only
		// deployments) or when refresh fails.
		fresh, err := importProvider.FreshAccessToken(ctx, jiraOAuthConfig(), providerName, *j.TriggeredBy)
		if err == nil && fresh != "" {
			return "Bearer " + fresh, nil
		}
		return "Bearer " + t.AccessToken, nil
	}
	if email == "" {
		return "", errors.New("jira token missing metadata.email; reconnect with email + API token")
	}
	raw := email + ":" + t.AccessToken
	return base64.StdEncoding.EncodeToString([]byte(raw)), nil
}

// resolveSite returns the full Jira API base URL (e.g., https://acme.atlassian.net).
// Pulled from token metadata.site_url. Cached per job id.
func (p *Provider) resolveSite(ctx context.Context, j *importModels.Job) (string, error) {
	p.mu.Lock()
	if cached, ok := p.siteCache[j.Id]; ok {
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	var md struct {
		SiteURL string `json:"site_url"`
	}
	_ = json.Unmarshal(t.Metadata, &md)
	site := strings.TrimRight(md.SiteURL, "/")
	if site == "" {
		return "", errors.New("jira token missing metadata.site_url; reconnect with the Atlassian site URL")
	}
	p.mu.Lock()
	p.siteCache[j.Id] = site
	p.mu.Unlock()
	return site, nil
}

// ─── helpers ──────────────────────────────────────────────────────

// parseJiraTime tolerates the multiple timestamp shapes Jira returns.
// Atlassian's docs claim a single RFC3339-style format with a 4-digit
// offset and no colon (e.g. 2025-05-24T10:11:22.987+0000) but real
// instances emit RFC3339Nano (with 'Z' or with colon offset) under
// different licensing tiers and timezone overrides. We try the most
// common shape first and fall back through the spec layouts.
//
// Returns the zero time on no match — callers default to time.Now().
func parseJiraTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000-0700",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05-0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// buildSourceTask converts a Jira issue to the generic SourceTask.
// issueFields are the fields a search asks for, besides custom ones.
const issueFields = "summary,description,status,priority,assignee,reporter,creator,issuetype,labels,duedate,created,updated,parent,subtasks,attachment"

func buildSourceTask(iss *jiraIssue, projectKey, site string, fields siteFields) importProvider.SourceTask {
	created := parseJiraTime(iss.Fields.Created)
	updated := parseJiraTime(iss.Fields.Updated)
	var due *time.Time
	if iss.Fields.DueDate != "" {
		if t, err := time.Parse("2006-01-02", iss.Fields.DueDate); err == nil {
			due = &t
		}
	}

	priority := ""
	if iss.Fields.Priority != nil {
		priority = iss.Fields.Priority.Name
	}

	assignees := []string{}
	if iss.Fields.Assignee != nil && iss.Fields.Assignee.AccountID != "" {
		assignees = append(assignees, iss.Fields.Assignee.AccountID)
	}

	body := ""
	if iss.RenderedFields != nil && iss.RenderedFields.Description != "" {
		body = iss.RenderedFields.Description
	} else if iss.Fields.Description != nil {
		body = htmlEscape(extractAtlassianText(iss.Fields.Description))
	}

	pkey := projectKey
	if pkey == "" {
		pkey = projectKeyFromIssue(iss)
	}

	atts := []importProvider.SourceAttachment{}
	for _, a := range iss.Fields.Attachment {
		atts = append(atts, importProvider.SourceAttachment{
			SourceID: a.ID,
			Name:     a.Filename,
			URL:      a.Content,
			Mime:     a.MimeType,
			Size:     a.Size,
			Parent:   importProvider.SourceRef{Kind: "task", SourceID: iss.Key},
		})
	}

	return importProvider.SourceTask{
		SourceID:        iss.Key,
		ProjectSourceID: pkey,
		Name:            iss.Fields.Summary,
		Description:     body,
		Status:          iss.Fields.Status.Name,
		Priority:        priority,
		Labels:          append([]string{}, iss.Fields.Labels...),
		AssigneeIds:     assignees,
		CreatedBy:       safeAccountID(iss.Fields.Creator),
		Created:         created,
		Updated:         updated,
		DueDate:         due,
		Completed:       isDoneStatus(iss.Fields.Status.Name),
		AttachmentRefs:  atts,
		Fields:          issueFieldValues(iss.Custom, fields),
		Metadata:        map[string]any{"jira_url": site + "/browse/" + iss.Key, "type": iss.Fields.IssueType.Name},
	}
}

func projectKeyFromIssue(iss *jiraIssue) string {
	if iss.Fields.Project != nil && iss.Fields.Project.Key != "" {
		return iss.Fields.Project.Key
	}
	if i := strings.IndexByte(iss.Key, '-'); i > 0 {
		return iss.Key[:i]
	}
	return ""
}

func isDoneStatus(s string) bool {
	switch strings.ToLower(s) {
	case "done", "closed", "resolved", "complete", "fixed":
		return true
	}
	return false
}

func safeAccountID(u *jiraUser) string {
	if u == nil {
		return ""
	}
	return u.AccountID
}

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

// extractAtlassianText walks an Atlassian Document Format (ADF) JSON
// blob and returns the concatenated text. We use this only as a
// fallback for descriptions/comments where renderedBody isn't
// available (shouldn't happen given expand=renderedFields, but defend).
//
// ADF is a tree of {type, content[], text}. This function is depth-
// first and tolerates unknown node types.
func extractAtlassianText(adf any) string {
	if adf == nil {
		return ""
	}
	var b strings.Builder
	walkADF(adf, &b)
	return b.String()
}

func walkADF(n any, b *strings.Builder) {
	switch v := n.(type) {
	case map[string]any:
		if t, ok := v["text"].(string); ok {
			b.WriteString(t)
		}
		if c, ok := v["content"].([]any); ok {
			for _, child := range c {
				walkADF(child, b)
			}
			// paragraph break
			if v["type"] == "paragraph" || v["type"] == "heading" {
				b.WriteByte('\n')
			}
		}
	case []any:
		for _, child := range v {
			walkADF(child, b)
		}
	}
}

// CleanupJob evicts the cached site URL for a finished job.
// Implements importProvider.JobCleaner.
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	p.mu.Lock()
	delete(p.siteCache, id)
	delete(p.fieldsCache, id)
	p.mu.Unlock()
}
