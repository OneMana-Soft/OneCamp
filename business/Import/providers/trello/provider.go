// Package trello implements the Import Provider interface for Trello.
//
// Two source modes are supported:
//
//  1. Live API ("api")        — uses the admin's API key + token. Fetches
//     the entire board JSON, plus per-card actions
//     (comments) and attachments via the REST API.
//  2. Board JSON ("board_json") — operator pastes the JSON exported from
//     Trello's "Show Menu → More → Print and
//     Export → Export as JSON" page. Same shape
//     as the API response, so one parser.
//
// Trello's data shape:
//   - Organization → Board → List → Card → Comment(action) → Attachment
//   - Cards have checklists with checkitems we surface as subtasks.
//   - Members are board members; admins are membership.idMember rows
//     where memberType=='admin'.
//
// In OneCamp:
//   - One synthetic Team per import workspace name (default "Trello").
//   - One Project per Board.
//   - One Task per Card. (Lists become statuses via mapping UI.)
//   - One Subtask per top-level checklist item.
//   - One Comment per commentCard action.
//   - Attachments fetched via the same URL pattern.
//
// Rate limits (Trello):
//   - 100 requests / 10s per API key, 300 / 10s per token.
//   - Plus a per-minute quota (4xx threshold).
//     We use a SleepLimiter ticking every 100ms with 10 burst — well under both.
package trello

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

const providerName = importModels.ProviderTrello

// Provider implements the importProvider.Provider interface for Trello.
//
// Stateless except for the rate limiter; safe to share across goroutines.
type Provider struct {
	rl importProvider.Limiter
}

// New constructs a Trello provider singleton. Called from init() and
// also by tests that want a fresh limiter.
func New() *Provider {
	return &Provider{
		// Trello docs allow 100/10s. 100ms ticks with 10 burst yields
		// ~100/sec sustained, but we conservatively cap to 600/min (every
		// 100ms) which is well under the platform limit.
		rl: importProvider.NewSleepLimiter(100*time.Millisecond, 10),
	}
}

func init() {
	importProvider.Register(New())
}

// Name returns the registry key.
func (p *Provider) Name() string { return providerName }

// Capabilities advertises what this provider produces.
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTeams |
		importProvider.CapProjects |
		importProvider.CapTasks |
		importProvider.CapSubtasks |
		importProvider.CapTaskComments |
		importProvider.CapAttachments
}

// SupportedSources tells the FE which upload modes are valid.
func (p *Provider) SupportedSources() []string {
	return []string{importModels.SourceAPI, importModels.SourceBoardJSON}
}

// Validate runs cheap checks. For api source we ping /members/me; for
// board_json we just verify the staged file is valid JSON with a name.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	switch j.Source {
	case importModels.SourceAPI:
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			return err
		}
		me, err := p.fetchMe(ctx, tok)
		if err != nil {
			return fmt.Errorf("trello auth: %w", err)
		}
		if me.ID == "" {
			return errors.New("trello /members/me returned empty id; bad credentials?")
		}
		return nil
	case importModels.SourceBoardJSON:
		_, err := p.loadStagedBoard(ctx, j)
		return err
	}
	return fmt.Errorf("unsupported source %q", j.Source)
}

// Plan walks the board enough to count tasks/comments/attachments and
// schedule the project chunk. We don't iterate cards twice — Plan does
// the count, the projects stage emits the chunks. So Plan returns a
// project-tasks chunk per board the operator wants imported.
//
// For api source: the operator picks a single board id via opts["board_id"].
// For board_json: there's exactly one board.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	board, err := p.resolveBoard(ctx, j, opts)
	if err != nil {
		return nil, nil, err
	}

	plan := &importProvider.Plan{
		ProjectCount: 1,
		Warnings:     []string{},
		StatusValues: collectListNames(board),
	}
	plan.UserCount = len(board.Members)
	for _, c := range board.Cards {
		if c.Closed {
			// Trello's "archived" state. We still import for completeness
			// and clamp to our "canceled" status via the default mapping.
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("Card %q is archived; will import as canceled", c.Name))
		}
		plan.TaskCount++
		plan.CommentCount += c.Badges.Comments
		plan.FileCount += c.Badges.Attachments
		// Top-level checkitems → subtasks.
		for _, cl := range c.Checklists {
			plan.SubtaskCount += len(cl.CheckItems)
		}
	}

	// Aggregate priority labels (Trello stars / labels) for the operator
	// to map. Trello has no native priority; people use labels named
	// "P1"/"High". We surface unique label names so the operator picks.
	labelSet := map[string]struct{}{}
	for _, l := range board.Labels {
		if l.Name != "" {
			labelSet[strings.ToLower(l.Name)] = struct{}{}
		}
	}
	for k := range labelSet {
		plan.PriorityValues = append(plan.PriorityValues, k)
	}

	return plan, nil, nil
}

// IterUsers streams board members.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("trello.IterUsers", errCh)
		board, err := p.resolveBoard(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, m := range board.Members {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceUser{
				SourceID:    m.ID,
				DisplayName: helpers.FirstNonEmpty(m.FullName, m.Username),
				Login:       m.Username,
				AvatarURL:   m.AvatarURL,
				Email:       "", // Trello doesn't expose member email via board export
				Metadata: map[string]any{
					"trello_username": m.Username,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits a single synthetic team per workspace.
// Trello has organisations, but the typical operator imports a single
// board so a synthetic default team keeps the model simple.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam)
	errCh := make(chan error, 1)
	go func() {
		// Empty stream: orchestrator falls back to the synthetic default team.
		close(out)
		close(errCh)
	}()
	return out, errCh
}

// IterProjects emits exactly one project (the board).
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 1)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("trello.IterProjects", errCh)
		board, err := p.resolveBoard(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		members := []string{}
		admins := []string{}
		for _, mb := range board.Memberships {
			members = append(members, mb.IDMember)
			if mb.MemberType == "admin" {
				admins = append(admins, mb.IDMember)
			}
		}
		fields, _ := boardFields(board.CustomFields)
		select {
		case <-ctx.Done():
			return
		case out <- importProvider.SourceProject{
			SourceID:    board.ID,
			Name:        board.Name,
			Description: board.Desc,
			MemberIds:   members,
			AdminIds:    admins,
			Fields:      fields,
			Metadata: map[string]any{
				"trello_url": board.URL,
			},
		}:
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams every card on the board. Trello "lists"
// become statuses via the operator-confirmed mapping.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("trello.IterTasksOfProject", errCh)
		board, err := p.resolveBoard(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		listName := map[string]string{}
		for _, l := range board.Lists {
			listName[l.ID] = l.Name
		}
		_, fieldByID := boardFields(board.CustomFields)
		for _, c := range board.Cards {
			if ctx.Err() != nil {
				return
			}
			status := listName[c.IDList]
			if c.Closed {
				status = "archived"
			}
			labels := []string{}
			priorityHint := ""
			for _, l := range c.Labels {
				if l.Name != "" {
					labels = append(labels, l.Name)
					priorityHint = l.Name // last label wins as a hint
				}
			}
			due := parseTrelloTime(c.Due)
			start := parseTrelloTime(c.Start)

			// Members on the card become assignees.
			assignees := append([]string{}, c.IDMembers...)

			// Schedule attachment refs from card attachments.
			// External (non-uploaded) Trello "attachments" are link
			// previews to off-platform URLs (Google Drive, GitHub PRs,
			// websites). We render them inline at the bottom of the
			// description rather than fetching binaries we don't own;
			// this preserves the link without producing a 404 on
			// download.
			atts := []importProvider.SourceAttachment{}
			linkRefs := []trelloAttachment{}
			for _, a := range c.Attachments {
				if a.URL == "" {
					continue
				}
				if !a.IsUpload {
					linkRefs = append(linkRefs, a)
					continue
				}
				atts = append(atts, importProvider.SourceAttachment{
					SourceID: a.ID,
					Name:     a.Name,
					URL:      a.URL,
					Mime:     a.MimeType,
					Size:     a.Bytes,
					Parent:   importProvider.SourceRef{Kind: "task", SourceID: c.ID},
				})
			}

			task := importProvider.SourceTask{
				SourceID:        c.ID,
				ProjectSourceID: board.ID,
				Name:            c.Name,
				Description:     trelloDescToHTML(c.Desc) + renderTrelloLinkRefs(linkRefs),
				Status:          status,
				Priority:        priorityHint,
				Labels:          labels,
				AssigneeIds:     assignees,
				StartDate:       start,
				DueDate:         due,
				Created:         parseTrelloIDTime(c.ID),
				Updated:         derefTime(parseTrelloTime(c.DateLastActivity)),
				Completed:       c.DueComplete || c.Closed,
				CommentCount:    c.Badges.Comments,
				SubtaskCount:    countCheckItems(c.Checklists),
				AttachmentRefs:  atts,
				Fields:          cardFieldValues(c.CustomFieldItems, fieldByID),
				Metadata: map[string]any{
					"trello_url": c.URL,
					"id_short":   c.IDShort,
					"closed":     c.Closed,
				},
			}
			select {
			case <-ctx.Done():
				return
			case out <- task:
			}
			// Emit a parent-task placeholder for each subtask root so
			// the orchestrator knows to schedule a subtasks chunk.
			if len(c.Checklists) > 0 {
				select {
				case <-ctx.Done():
					return
				case out <- importProvider.SourceTask{
					SourceID:     "__sub__:" + c.ID, // synthetic
					ParentTaskID: c.ID,
				}:
				}
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask streams checklist items for one parent card.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("trello.IterSubtasksOfTask", errCh)
		board, err := p.resolveBoard(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		var card *trelloCard
		for i := range board.Cards {
			if board.Cards[i].ID == taskSourceId {
				card = &board.Cards[i]
				break
			}
		}
		if card == nil {
			return
		}
		for _, cl := range card.Checklists {
			for _, ci := range cl.CheckItems {
				if ctx.Err() != nil {
					return
				}
				status := "todo"
				if ci.State == "complete" {
					status = "done"
				}
				select {
				case <-ctx.Done():
					return
				case out <- importProvider.SourceTask{
					SourceID:        ci.ID,
					ParentTaskID:    card.ID,
					ProjectSourceID: card.IDBoard,
					Name:            ci.Name,
					Status:          status,
					Completed:       ci.State == "complete",
					Created:         parseTrelloIDTime(ci.ID),
				}:
				}
			}
		}
	}()
	return out, errCh
}

// IterCommentsOfTask streams comments for one card. For api source we
// first try the cached board.Actions (which fetchBoard already loaded
// with actions=commentCard&actions_limit=1000), then fall back to a
// per-card /cards/{id}/actions paginate when the cached count hit
// the 1000 cap (i.e. board has more than 1000 comments total). For
// board_json the comments are inside board.Actions.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("trello.IterCommentsOfTask", errCh)
		var actions []trelloAction
		// First, look in the cached board snapshot: this avoids a
		// per-card HTTP round-trip when the board has < 1000 comments.
		board, err := p.resolveBoard(ctx, j, opts)
		if err != nil {
			errCh <- err
			return
		}
		for _, a := range board.Actions {
			if a.Data.Card.ID == taskSourceId {
				actions = append(actions, a)
			}
		}
		// Live API + board hit the 1000-action cap → paginate per card.
		if j.Source == importModels.SourceAPI && len(board.Actions) >= 1000 {
			tok, err := p.loadToken(ctx, j)
			if err != nil {
				errCh <- err
				return
			}
			as, err := p.fetchCardActions(ctx, tok, taskSourceId)
			if err != nil {
				errCh <- err
				return
			}
			actions = as
		}
		for _, a := range actions {
			if a.Type != "commentCard" {
				continue
			}
			created, _ := time.Parse(time.RFC3339, a.Date)
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceComment{
				SourceID:       a.ID,
				TaskSourceID:   taskSourceId,
				Body:           trelloDescToHTML(a.Data.Text),
				AuthorSourceID: a.IDMemberCreator,
				Created:        created,
			}:
			}
		}
	}()
	return out, errCh
}

// FetchAttachment streams a Trello attachment to dest. Trello attachments
// served from trello.com require the API key + token in the URL when
// the board is private.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", 0, err
	}
	att.URL = trelloDownloadURL(att.URL, tok)
	return importProvider.DefaultFetchAttachment(ctx, att, dest)
}

// trelloHost serves the files uploaded to Trello, which need the importing
// admin's key and token on a private board. Nothing else may be sent them; the
// check used to be a substring search, so "https://evil.example/trello.com"
// got both.
const trelloHost = "trello.com"

// trelloDownloadURL is raw with the key and token added when it is a Trello
// URL, and as it is when it isn't.
func trelloDownloadURL(raw string, tok *trelloToken) string {
	if tok == nil || !importProvider.CredentialAllowed(raw, trelloHost) {
		return raw
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + fmt.Sprintf("%skey=%s&token=%s", sep, tok.APIKey, tok.Token)
}

// DefaultStatusMap returns the proposed Trello-list-name → OneCamp
// status map. Operators override per-import in the Plan UI.
//
// This is intentionally conservative: known names map cleanly, and
// custom list names fall through to "todo" via the heuristic.
func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		"backlog":     "backlog",
		"to do":       "todo",
		"todo":        "todo",
		"open":        "todo",
		"doing":       "inProgress",
		"in progress": "inProgress",
		"in-progress": "inProgress",
		"in review":   "inReview",
		"review":      "inReview",
		"qa":          "inReview",
		"done":        "done",
		"completed":   "done",
		"archived":    "canceled",
	}
}

// DefaultPriorityMap maps common Trello label names to OneCamp priorities.
func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"p1":       "high",
		"p2":       "medium",
		"p3":       "low",
		"p4":       "low",
		"high":     "high",
		"medium":   "medium",
		"low":      "low",
		"urgent":   "high",
		"critical": "high",
	}
}

// ─── Trello DTOs (subset we use) ──────────────────────────────────

type trelloMember struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FullName  string `json:"fullName"`
	AvatarURL string `json:"avatarUrl"`
}

type trelloMembership struct {
	ID         string `json:"id"`
	IDMember   string `json:"idMember"`
	MemberType string `json:"memberType"`
}

type trelloLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type trelloList struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Closed bool   `json:"closed"`
}

type trelloCheckItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type trelloChecklist struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	CheckItems []trelloCheckItem `json:"checkItems"`
}

type trelloAttachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	Bytes    int64  `json:"bytes"`
	IsUpload bool   `json:"isUpload"`
}

type trelloBadges struct {
	Comments    int `json:"comments"`
	Attachments int `json:"attachments"`
}

type trelloCard struct {
	ID               string                  `json:"id"`
	IDShort          int                     `json:"idShort"`
	IDList           string                  `json:"idList"`
	IDBoard          string                  `json:"idBoard"`
	Name             string                  `json:"name"`
	Desc             string                  `json:"desc"`
	URL              string                  `json:"url"`
	Closed           bool                    `json:"closed"`
	Due              string                  `json:"due"`
	Start            string                  `json:"start"`
	DueComplete      bool                    `json:"dueComplete"`
	DateLastActivity string                  `json:"dateLastActivity"`
	IDMembers        []string                `json:"idMembers"`
	Labels           []trelloLabel           `json:"labels"`
	Checklists       []trelloChecklist       `json:"checklists"`
	Attachments      []trelloAttachment      `json:"attachments"`
	Badges           trelloBadges            `json:"badges"`
	CustomFieldItems []trelloCustomFieldItem `json:"customFieldItems"`
}

type trelloActionData struct {
	Text string `json:"text"`
	Card struct {
		ID string `json:"id"`
	} `json:"card"`
}

type trelloAction struct {
	ID              string           `json:"id"`
	Type            string           `json:"type"`
	Date            string           `json:"date"`
	IDMemberCreator string           `json:"idMemberCreator"`
	Data            trelloActionData `json:"data"`
}

type trelloBoard struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Desc         string              `json:"desc"`
	URL          string              `json:"url"`
	Members      []trelloMember      `json:"members"`
	Memberships  []trelloMembership  `json:"memberships"`
	Labels       []trelloLabel       `json:"labels"`
	Lists        []trelloList        `json:"lists"`
	Cards        []trelloCard        `json:"cards"`
	Actions      []trelloAction      `json:"actions"`
	CustomFields []trelloCustomField `json:"customFields"`
}

// ─── HTTP + token helpers ────────────────────────────────────────

// trelloToken bundles the API key (admin-app constant) and the per-user
// token. Stored as a single ConnectRequest with the api_key in metadata.
type trelloToken struct {
	APIKey string
	Token  string
}

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (*trelloToken, error) {
	if j.TriggeredBy == nil {
		return nil, errors.New("job has no triggered_by user")
	}
	tok, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return nil, err
	}
	apiKey := ""
	if len(tok.Metadata) > 0 {
		var md struct {
			APIKey string `json:"api_key"`
		}
		_ = json.Unmarshal(tok.Metadata, &md)
		apiKey = md.APIKey
	}
	if apiKey == "" {
		return nil, errors.New("trello token missing api_key in metadata; reconnect")
	}
	return &trelloToken{APIKey: apiKey, Token: tok.AccessToken}, nil
}

type trelloMe struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	FullName string `json:"fullName"`
}

func (p *Provider) fetchMe(ctx context.Context, tok *trelloToken) (*trelloMe, error) {
	u := fmt.Sprintf("https://api.trello.com/1/members/me?key=%s&token=%s",
		url.QueryEscape(tok.APIKey), url.QueryEscape(tok.Token))
	var m trelloMe
	if err := p.getJSON(ctx, u, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// fetchBoard pulls the full board with all the embedded objects we need.
// Trello supports nested expansion via query parameters; this is what
// the official "Export as JSON" endpoint uses internally.
func (p *Provider) fetchBoard(ctx context.Context, tok *trelloToken, boardId string) (*trelloBoard, error) {
	u := fmt.Sprintf("https://api.trello.com/1/boards/%s?"+
		"members=all&member_fields=id,username,fullName,avatarUrl"+
		"&memberships=all"+
		"&lists=all"+
		"&cards=all&card_fields=all"+
		"&card_attachments=true&card_attachment_fields=id,name,url,mimeType,bytes,isUpload"+
		"&card_checklists=all"+
		"&actions=commentCard&actions_limit=1000"+
		"&labels=all"+
		"&customFields=true&card_customFieldItems=true"+
		"&key=%s&token=%s",
		url.PathEscape(boardId),
		url.QueryEscape(tok.APIKey),
		url.QueryEscape(tok.Token),
	)
	var b trelloBoard
	if err := p.getJSON(ctx, u, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// fetchCardActions paginates commentCard actions for one card.
func (p *Provider) fetchCardActions(ctx context.Context, tok *trelloToken, cardId string) ([]trelloAction, error) {
	out := []trelloAction{}
	beforeDate := ""
	for page := 0; page < 50; page++ { // safety cap
		u := fmt.Sprintf("https://api.trello.com/1/cards/%s/actions?filter=commentCard&limit=1000&key=%s&token=%s",
			url.PathEscape(cardId),
			url.QueryEscape(tok.APIKey),
			url.QueryEscape(tok.Token))
		if beforeDate != "" {
			u += "&before=" + url.QueryEscape(beforeDate)
		}
		var page []trelloAction
		if err := p.getJSON(ctx, u, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		out = append(out, page...)
		// Trello returns newest first; paginate via `before=oldest_date`.
		beforeDate = page[len(page)-1].Date
		if len(page) < 1000 {
			break
		}
	}
	return out, nil
}

// getJSON wraps GET + JSON-decode + rate limit + 429 handling.
func (p *Provider) getJSON(ctx context.Context, urlStr string, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 5 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return &importProvider.ErrRateLimited{RetryAfter: retryAfter, Reason: "Trello 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &importProvider.TokenRejected{Msg: fmt.Sprintf("trello auth failed (HTTP %d); reconnect token", resp.StatusCode)}
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("trello HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ─── Source-mode resolvers ────────────────────────────────────────

// resolveBoard returns the board JSON regardless of source mode.
// Memoised on the orchestrator's context via a per-job cache key would
// be ideal, but each goroutine pulls independently so we trade a bit of
// re-fetching for simpler code. Trello board JSON for typical workspaces
// is < 5 MB.
func (p *Provider) resolveBoard(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*trelloBoard, error) {
	if cached := boardCache.Load(j.Id); cached != nil {
		return cached, nil
	}
	var b *trelloBoard
	var err error
	switch j.Source {
	case importModels.SourceAPI:
		boardId, _ := opts["board_id"].(string)
		if boardId == "" {
			return nil, errors.New("opts.board_id required for trello live API")
		}
		tok, err := p.loadToken(ctx, j)
		if err != nil {
			return nil, err
		}
		b, err2 := p.fetchBoard(ctx, tok, boardId)
		if err2 != nil {
			return nil, err2
		}
		boardCache.Store(j.Id, b)
		return b, nil
	case importModels.SourceBoardJSON:
		b, err = p.loadStagedBoard(ctx, j)
		if err != nil {
			return nil, err
		}
		boardCache.Store(j.Id, b)
		return b, nil
	}
	return nil, fmt.Errorf("unsupported source %q", j.Source)
}

// loadStagedBoard reads the staged JSON object from MinIO and decodes it.
// We use the slack import's MinIO range reader because it's already
// generic; for Trello board JSON the file is small enough to read in
// one shot, but the helper handles both cases.
func (p *Provider) loadStagedBoard(ctx context.Context, j *importModels.Job) (*trelloBoard, error) {
	if j.RawObjectKey == nil {
		return nil, errors.New("job has no raw_object_key")
	}
	rdr, cleanup, err := openStagedFile(ctx, *j.RawObjectKey)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var b trelloBoard
	dec := json.NewDecoder(rdr)
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("decode trello board json: %w", err)
	}
	if b.ID == "" {
		return nil, errors.New("trello board json missing id")
	}
	return &b, nil
}

// ─── Helpers ──────────────────────────────────────────────────────

// trelloDescToHTML converts Trello's markdown to a tiny HTML subset.
// We do not use a full markdown library to keep the dependency surface
// small; this mirrors the conservative renderer in the Slack mrkdwn path.
func trelloDescToHTML(s string) string {
	if s == "" {
		return ""
	}
	out := htmlEscape(strings.TrimSpace(s))
	// Newlines → <br>. Trello rarely uses paragraph blocks.
	out = strings.ReplaceAll(out, "\n", "<br>")
	return out
}

// htmlEscape covers <, >, &, ", '.
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

// renderTrelloLinkRefs appends a small "Linked attachments" footer to
// the description for every external (isUpload=false) attachment on
// a Trello card. We don't try to fetch the binary because it lives
// off-platform (Google Drive, GitHub PRs, raw websites); preserving
// the link as a clickable HTML anchor matches the Trello UI's "linked
// attachments" rail and keeps zero attachment download failures for
// these refs.
func renderTrelloLinkRefs(refs []trelloAttachment) string {
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<hr><p><em>Linked attachments (imported as links):</em></p><ul>`)
	for _, a := range refs {
		name := a.Name
		if name == "" {
			name = a.URL
		}
		b.WriteString(`<li><a href="`)
		b.WriteString(htmlEscape(a.URL))
		b.WriteString(`" target="_blank" rel="noopener noreferrer">`)
		b.WriteString(htmlEscape(name))
		b.WriteString(`</a></li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

func parseTrelloTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// derefTime returns t deref'd or zero. Used when SourceTask wants a
// concrete time.Time and parseTrelloTime returned nil.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// parseTrelloIDTime extracts the embedded creation timestamp from a
// Trello object id. The first 8 hex chars are unix seconds. Returns
// zero time on failure (caller treats as "no created date").
func parseTrelloIDTime(id string) time.Time {
	if len(id) < 8 {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(id[:8], 16, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func collectListNames(b *trelloBoard) []string {
	out := make([]string, 0, len(b.Lists)+1)
	seen := map[string]struct{}{}
	for _, l := range b.Lists {
		k := strings.ToLower(l.Name)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, l.Name)
	}
	if hasArchived(b) {
		out = append(out, "archived")
	}
	return out
}

func hasArchived(b *trelloBoard) bool {
	for _, c := range b.Cards {
		if c.Closed {
			return true
		}
	}
	return false
}

func countCheckItems(cls []trelloChecklist) int {
	n := 0
	for _, cl := range cls {
		n += len(cl.CheckItems)
	}
	return n
}

// Per-job board memo. Key never repeats (uuid v4) so we don't need
// eviction; jobs that finish leave at most one entry behind, and the
// process restarts on every release. Read/write is mutex-protected
// because IterUsers / IterProjects / IterTasksOfProject can run in
// parallel goroutines spawned by the orchestrator.
var boardCache = newJobCache[*trelloBoard]()

// ─── Tiny generic per-job cache (concurrency-safe) ─────────────────

type jobCache[T any] struct {
	mu sync.RWMutex
	m  map[uuid.UUID]T
}

func newJobCache[T any]() *jobCache[T] {
	return &jobCache[T]{m: map[uuid.UUID]T{}}
}

func (c *jobCache[T]) Load(id uuid.UUID) T {
	c.mu.RLock()
	v := c.m[id]
	c.mu.RUnlock()
	return v
}

func (c *jobCache[T]) Store(id uuid.UUID, v T) {
	c.mu.Lock()
	c.m[id] = v
	c.mu.Unlock()
}

// Evict removes a job's cached entry. Called by the orchestrator when
// a job reaches a terminal state so the in-process map doesn't grow
// unbounded across long-running deploys.
func (c *jobCache[T]) Evict(id uuid.UUID) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// CleanupJob evicts the cached board snapshot for a finished job.
// Implements importProvider.JobCleaner.
func (p *Provider) CleanupJob(jobId string) {
	id, err := uuid.Parse(jobId)
	if err != nil {
		return
	}
	boardCache.Evict(id)
}
