// Package basecamp implements the Import Provider interface for Basecamp 5
// (basecamp-46): a company moves in an afternoon.
//
// Source: the live bc3 API only. Auth: signing in through Launchpad
// (oauth.go). The admin picks the Basecamp account to import (a sign-in can
// see several); the job carries its id (options.account_id).
//
// Mapping:
//
//	Person               → user; a client is external, an agent a bot, a
//	                       tombstone (someone deleted) nobody
//	Project              → project, with its people; archived stays archived
//	To-do                → task, open or done; its list (and group) as tags
//	                       until named lists land (basecamp-03)
//	Card                 → task, its column as its status; the Done column
//	                       completes it
//	Step (subtask)       → sub-task
//	Comment              → comment, with its files
//	File in a to-do's or
//	card's text          → attachment
//
// Not yet (they need the content interface, B-1b, with chat, docs and
// calendar): the Message Board, Chat, Docs & Files, the Schedule, Check-ins
// and Email Forwards. On Hold isn't in the API, so a card on hold comes in
// its column.
package basecamp

import (
	"context"
	"errors"
	"fmt"
	"io"
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

const providerName = importModels.ProviderBasecamp

// A to-do's status as the import's status map sees it; a card's is its
// column's title.
const (
	statusOpen = "to-do"
	statusDone = "done"
)

// Provider implements importProvider.Provider for Basecamp.
type Provider struct {
	rl importProvider.Limiter
	// token is the job's access token; a field so tests can stand one in.
	token func(ctx context.Context, j *importModels.Job) (string, error)

	mu       sync.Mutex
	accounts map[uuid.UUID]string // job → the account's API base
}

func New() *Provider {
	p := &Provider{
		// About 50 requests per 10 seconds per IP: one every 200ms, 10 at once.
		rl:       importProvider.NewSleepLimiter(200*time.Millisecond, 10),
		accounts: make(map[uuid.UUID]string, 4),
	}
	p.token = p.loadToken
	return p
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
		statusOpen:    "todo",
		statusDone:    "done",
		"triage":      "todo",
		"not now":     "todo",
		"in progress": "inProgress",
	}
}

// DefaultPriorityMap is empty: Basecamp has no priorities.
func (p *Provider) DefaultPriorityMap() map[string]string { return map[string]string{} }

// ─── bc3 shapes (the fields read) ─────────────────────────────────────

type bcRef struct {
	ID int64 `json:"id"`
}

type bcPerson struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Email          string `json:"email_address"`
	PersonableType string `json:"personable_type"`
	Client         bool   `json:"client"`
	AvatarURL      string `json:"avatar_url"`
	Company        *struct {
		Name string `json:"name"`
	} `json:"company"`
}

type bcDock struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

type bcProject struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	AppURL      string    `json:"app_url"`
	Dock        []bcDock  `json:"dock"`
	archived    bool
}

type bcTodolist struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	CompletedRatio string `json:"completed_ratio"`
	TodosURL       string `json:"todos_url"`
	GroupsURL      string `json:"groups_url"`
}

type bcFileRef struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	DownloadURL string `json:"download_url"`
}

// bcRecording is a to-do, a card or a step.
type bcRecording struct {
	ID                     int64       `json:"id"`
	Title                  string      `json:"title"`
	Description            string      `json:"description"`
	Content                string      `json:"content"`
	Completed              bool        `json:"completed"`
	StartsOn               string      `json:"starts_on"`
	DueOn                  string      `json:"due_on"`
	Creator                *bcRef      `json:"creator"`
	Assignees              []bcRef     `json:"assignees"`
	CommentsCount          int         `json:"comments_count"`
	SubtasksCount          int         `json:"subtasks_count"`
	CreatedAt              time.Time   `json:"created_at"`
	UpdatedAt              time.Time   `json:"updated_at"`
	AppURL                 string      `json:"app_url"`
	DescriptionAttachments []bcFileRef `json:"description_attachments"`
}

type bcColumn struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	CardsCount int    `json:"cards_count"`
	CardsURL   string `json:"cards_url"`
}

type bcCardTable struct {
	ID    int64      `json:"id"`
	Lists []bcColumn `json:"lists"`
}

type bcComment struct {
	ID                 int64       `json:"id"`
	Content            string      `json:"content"`
	Creator            *bcRef      `json:"creator"`
	CreatedAt          time.Time   `json:"created_at"`
	ContentAttachments []bcFileRef `json:"content_attachments"`
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// ─── Account, token, discovery ────────────────────────────────────────

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	return importProvider.FreshAccessToken(ctx, oauthConfig(), providerName, *j.TriggeredBy)
}

// accountsOf are the Basecamp accounts a sign-in can see (not its HEY or
// older Basecamp ones), each with its API base as URL.
func (p *Provider) accountsOf(ctx context.Context, tok string) ([]importProvider.DiscoverItem, error) {
	var auth struct {
		Accounts []struct {
			Product string `json:"product"`
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Href    string `json:"href"`
			AppHref string `json:"app_href"`
		} `json:"accounts"`
	}
	if _, err := p.get(ctx, tok, launchpadBase+"/authorization.json", &auth); err != nil {
		return nil, err
	}
	var out []importProvider.DiscoverItem
	for _, a := range auth.Accounts {
		if a.Product == "bc3" {
			out = append(out, importProvider.DiscoverItem{ID: id(a.ID), Name: a.Name, Kind: "account", URL: a.AppHref,
				Meta: map[string]any{"api": strings.TrimRight(a.Href, "/")}})
		}
	}
	return out, nil
}

// Discover lists the Basecamp accounts the sign-in can import.
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	return p.accountsOf(ctx, token.AccessToken)
}

// account is the job's token and the API base of the account it imports.
func (p *Provider) account(ctx context.Context, j *importModels.Job) (string, string, error) {
	tok, err := p.token(ctx, j)
	if err != nil {
		return "", "", err
	}
	p.mu.Lock()
	base, ok := p.accounts[j.Id]
	p.mu.Unlock()
	if ok {
		return tok, base, nil
	}
	items, err := p.accountsOf(ctx, tok)
	if err != nil {
		return "", "", err
	}
	picked, err := importProvider.PickScope(items, importProvider.JobPickedID(j, importProvider.OptBasecampAccountID), j.SourceWorkspaceName,
		importProvider.Scope{Provider: "Basecamp", Noun: "account", Credential: "sign-in"})
	if err != nil {
		return "", "", err
	}
	for _, it := range items {
		if it.ID == picked {
			base, _ = it.Meta["api"].(string)
		}
	}
	p.mu.Lock()
	p.accounts[j.Id] = base
	p.mu.Unlock()
	return tok, base, nil
}

// CleanupJob forgets the job's account.
func (p *Provider) CleanupJob(jobId string) {
	if u, err := uuid.Parse(jobId); err == nil {
		p.mu.Lock()
		delete(p.accounts, u)
		p.mu.Unlock()
	}
}

// Validate confirms the sign-in still works and sees the picked account.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	_, _, err := p.account(ctx, j)
	return err
}

// ─── Reads ────────────────────────────────────────────────────────────

func (p *Provider) people(ctx context.Context, tok, url string) ([]bcPerson, error) {
	var all []bcPerson
	if err := p.getAll(ctx, tok, url, &all); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, pr := range all {
		if pr.PersonableType != "Tombstone" {
			out = append(out, pr)
		}
	}
	return out, nil
}

// projects are the account's projects, active then archived.
func (p *Provider) projects(ctx context.Context, tok, base string) ([]bcProject, error) {
	var active, archived []bcProject
	if err := p.getAll(ctx, tok, base+"/projects.json", &active); err != nil {
		return nil, err
	}
	if err := p.getAll(ctx, tok, base+"/projects.json?status=archived", &archived); err != nil {
		return nil, err
	}
	for i := range archived {
		archived[i].archived = true
	}
	return append(active, archived...), nil
}

// tools are a project's turned-on tools of one kind; a project can have
// more than one of a kind.
func tools(pr bcProject, name string) []bcDock {
	var out []bcDock
	for _, d := range pr.Dock {
		if d.Name == name && d.Enabled {
			out = append(out, d)
		}
	}
	return out
}

func (p *Provider) todolists(ctx context.Context, tok, base string, todoset bcDock) ([]bcTodolist, error) {
	var lists []bcTodolist
	err := p.getAll(ctx, tok, fmt.Sprintf("%s/todosets/%d/todolists.json", base, todoset.ID), &lists)
	return lists, err
}

func (p *Provider) cardTable(ctx context.Context, tok string, board bcDock) (bcCardTable, error) {
	var t bcCardTable
	_, err := p.get(ctx, tok, board.URL, &t)
	return t, err
}

// todosOf are a list's to-dos, open then done.
func (p *Provider) todosOf(ctx context.Context, tok, url string) ([]bcRecording, error) {
	var open, done []bcRecording
	if err := p.getAll(ctx, tok, url, &open); err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	if err := p.getAll(ctx, tok, url+sep+"completed=true", &done); err != nil {
		return nil, err
	}
	return append(open, done...), nil
}

// ─── Plan ─────────────────────────────────────────────────────────────

// listTotal is how many to-dos a list holds, from its "done/total" ratio.
func listTotal(ratio string) int {
	_, total, ok := strings.Cut(ratio, "/")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(total))
	return n
}

// Plan counts the people, projects and tasks, and offers each card column as
// a status to map, before anything is written.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	tok, base, err := p.account(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	people, err := p.people(ctx, tok, base+"/people.json")
	if err != nil {
		return nil, nil, fmt.Errorf("basecamp people: %w", err)
	}
	projects, err := p.projects(ctx, tok, base)
	if err != nil {
		return nil, nil, fmt.Errorf("basecamp projects: %w", err)
	}
	plan := &importProvider.Plan{UserCount: len(people), ProjectCount: len(projects)}
	statuses := map[string]bool{statusOpen: true, statusDone: true}
	for _, pr := range projects {
		for _, set := range tools(pr, "todoset") {
			lists, err := p.todolists(ctx, tok, base, set)
			if err != nil {
				return nil, nil, err
			}
			for _, l := range lists {
				plan.TaskCount += listTotal(l.CompletedRatio)
			}
		}
		for _, board := range tools(pr, "kanban_board") {
			t, err := p.cardTable(ctx, tok, board)
			if err != nil {
				return nil, nil, err
			}
			for _, c := range t.Lists {
				plan.TaskCount += c.CardsCount
				statuses[c.Title] = true
			}
		}
	}
	for s := range statuses {
		plan.StatusValues = append(plan.StatusValues, s)
	}
	plan.Warnings = append(plan.Warnings, "Message boards, chat, docs and files, schedules and check-ins aren't brought across yet; to-dos, cards, their steps, comments and files are.")
	return plan, nil, nil
}

// ─── Iterators ────────────────────────────────────────────────────────

func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterUsers", func() ([]importProvider.SourceUser, error) {
		tok, base, err := p.account(ctx, j)
		if err != nil {
			return nil, err
		}
		people, err := p.people(ctx, tok, base+"/people.json")
		if err != nil {
			return nil, err
		}
		out := make([]importProvider.SourceUser, 0, len(people))
		for _, pr := range people {
			u := importProvider.SourceUser{
				SourceID:    id(pr.ID),
				Email:       pr.Email,
				DisplayName: pr.Name,
				AvatarURL:   pr.AvatarURL,
				IsBot:       pr.PersonableType == "Agent" || pr.PersonableType == "Integration",
				IsExternal:  pr.Client,
			}
			if pr.Company != nil {
				u.Metadata = map[string]any{"company": pr.Company.Name}
			}
			out = append(out, u)
		}
		return out, nil
	})
}

// IterTeams is empty: Basecamp has no teams, so projects fall under the
// import's default team.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterTeams", func() ([]importProvider.SourceTeam, error) { return nil, nil })
}

func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterProjects", func() ([]importProvider.SourceProject, error) {
		tok, base, err := p.account(ctx, j)
		if err != nil {
			return nil, err
		}
		projects, err := p.projects(ctx, tok, base)
		if err != nil {
			return nil, err
		}
		out := make([]importProvider.SourceProject, 0, len(projects))
		for _, pr := range projects {
			people, err := p.people(ctx, tok, fmt.Sprintf("%s/projects/%d/people.json", base, pr.ID))
			if err != nil {
				return nil, err
			}
			members := make([]string, 0, len(people))
			for _, m := range people {
				members = append(members, id(m.ID))
			}
			out = append(out, importProvider.SourceProject{
				SourceID:    id(pr.ID),
				Name:        pr.Name,
				Description: pr.Description,
				MemberIds:   members,
				Created:     pr.CreatedAt,
				Archived:    pr.archived,
				Metadata:    map[string]any{"basecamp_url": pr.AppURL},
			})
		}
		return out, nil
	})
}

// IterTasksOfProject streams a project's to-dos and cards.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterTasksOfProject", func() ([]importProvider.SourceTask, error) {
		tok, base, err := p.account(ctx, j)
		if err != nil {
			return nil, err
		}
		var pr bcProject
		if _, err := p.get(ctx, tok, base+"/projects/"+projectSourceId+".json", &pr); err != nil {
			return nil, err
		}
		var out []importProvider.SourceTask
		seen := map[int64]bool{}
		add := func(r bcRecording, status string, done bool, labels ...string) {
			if seen[r.ID] {
				return
			}
			seen[r.ID] = true
			out = append(out, toTask(r, projectSourceId, status, done, labels))
		}
		for _, set := range tools(pr, "todoset") {
			lists, err := p.todolists(ctx, tok, base, set)
			if err != nil {
				return nil, err
			}
			for _, l := range lists {
				// Groups first, so a to-do in a group keeps both names
				// whether or not its list lists it too.
				var groups []bcTodolist
				if l.GroupsURL != "" {
					if err := p.getAll(ctx, tok, l.GroupsURL, &groups); err != nil {
						return nil, err
					}
				}
				for _, g := range groups {
					todos, err := p.todosOf(ctx, tok, g.TodosURL)
					if err != nil {
						return nil, err
					}
					for _, t := range todos {
						add(t, todoStatus(t), t.Completed, l.Title, g.Title)
					}
				}
				todos, err := p.todosOf(ctx, tok, l.TodosURL)
				if err != nil {
					return nil, err
				}
				for _, t := range todos {
					add(t, todoStatus(t), t.Completed, l.Title)
				}
			}
		}
		for _, board := range tools(pr, "kanban_board") {
			table, err := p.cardTable(ctx, tok, board)
			if err != nil {
				return nil, err
			}
			for _, col := range table.Lists {
				var cards []bcRecording
				if err := p.getAll(ctx, tok, col.CardsURL, &cards); err != nil {
					return nil, err
				}
				for _, c := range cards {
					add(c, col.Title, c.Completed || col.Type == "Kanban::DoneColumn")
				}
			}
		}
		return out, nil
	})
}

// IterSubtasksOfTask streams a to-do's or card's steps.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceTask, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterSubtasksOfTask", func() ([]importProvider.SourceTask, error) {
		tok, base, err := p.account(ctx, j)
		if err != nil {
			return nil, err
		}
		var steps []bcRecording
		if err := p.getAll(ctx, tok, base+"/recordings/"+taskSourceId+"/subtasks.json", &steps); err != nil {
			return nil, err
		}
		out := make([]importProvider.SourceTask, 0, len(steps))
		for _, s := range steps {
			st := toTask(s, "", todoStatus(s), s.Completed, nil)
			st.ParentTaskID = taskSourceId
			out = append(out, st)
		}
		return out, nil
	})
}

func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceId string) (<-chan importProvider.SourceComment, <-chan error) {
	return importProvider.Stream(ctx, "basecamp.IterCommentsOfTask", func() ([]importProvider.SourceComment, error) {
		tok, base, err := p.account(ctx, j)
		if err != nil {
			return nil, err
		}
		var comments []bcComment
		if err := p.getAll(ctx, tok, base+"/recordings/"+taskSourceId+"/comments.json", &comments); err != nil {
			return nil, err
		}
		out := make([]importProvider.SourceComment, 0, len(comments))
		for _, c := range comments {
			sc := importProvider.SourceComment{
				SourceID:       id(c.ID),
				TaskSourceID:   taskSourceId,
				Body:           richText(c.Content),
				Created:        c.CreatedAt,
				AttachmentRefs: files(c.ContentAttachments, importProvider.SourceRef{Kind: "comment", SourceID: id(c.ID)}),
			}
			if c.Creator != nil {
				sc.AuthorSourceID = id(c.Creator.ID)
			}
			out = append(out, sc)
		}
		return out, nil
	})
}

// basecampStorage are the hosts a file may be fetched from with the sign-in.
var basecampStorage = []string{"basecamp.com", "basecampapi.com"}

// bcFile is att as it is fetched: with the sign-in when it is a Basecamp
// file, and without when it isn't.
func bcFile(att importProvider.SourceAttachment, tok string) importProvider.SourceAttachment {
	return importProvider.WithAuthorization(att, "Bearer "+tok, basecampStorage...)
}

func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.token(ctx, j)
	if err != nil {
		return "", 0, err
	}
	return importProvider.DefaultFetchAttachment(ctx, bcFile(att, tok), dest)
}

// ─── Mapping ──────────────────────────────────────────────────────────

func todoStatus(r bcRecording) string {
	if r.Completed {
		return statusDone
	}
	return statusOpen
}

func day(s string) *time.Time {
	if s == "" {
		return nil
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil
	}
	return &d
}

func files(refs []bcFileRef, parent importProvider.SourceRef) []importProvider.SourceAttachment {
	out := make([]importProvider.SourceAttachment, 0, len(refs))
	for _, f := range refs {
		if f.DownloadURL == "" {
			continue
		}
		out = append(out, importProvider.SourceAttachment{
			SourceID: id(f.ID),
			Name:     f.Filename,
			URL:      f.DownloadURL,
			Mime:     f.ContentType,
			Size:     f.ByteSize,
			Parent:   parent,
		})
	}
	return out
}

func toTask(r bcRecording, projectID, status string, done bool, labels []string) importProvider.SourceTask {
	body := r.Description
	if strings.TrimSpace(body) == "" {
		body = r.Content
	}
	st := importProvider.SourceTask{
		SourceID:        id(r.ID),
		ProjectSourceID: projectID,
		Name:            helpers.TruncateRunes(r.Title, 256),
		Description:     richText(body),
		Status:          status,
		Labels:          labels,
		StartDate:       day(r.StartsOn),
		DueDate:         day(r.DueOn),
		Created:         r.CreatedAt,
		Updated:         r.UpdatedAt,
		Completed:       done,
		AttachmentRefs:  files(r.DescriptionAttachments, importProvider.SourceRef{Kind: "task", SourceID: id(r.ID)}),
		CommentCount:    r.CommentsCount,
		SubtaskCount:    r.SubtasksCount,
		Metadata:        map[string]any{"basecamp_url": r.AppURL},
	}
	for _, a := range r.Assignees {
		st.AssigneeIds = append(st.AssigneeIds, id(a.ID))
	}
	if r.Creator != nil {
		st.CreatedBy = id(r.Creator.ID)
	}
	return st
}

var (
	bcAttachment = regexp.MustCompile(`(?s)<bc-attachment\b([^>]*?)(?:/>|>(.*?)</bc-attachment>)`)
	anyTag       = regexp.MustCompile(`<[^>]*>`)
)

// richText is Basecamp's rich text as a task holds it. A file in the text
// comes across as the task's file, so its embed goes (its link needs the
// sign-in, and would show broken); a mention stays as the name.
func richText(html string) string {
	return bcAttachment.ReplaceAllStringFunc(html, func(m string) string {
		parts := bcAttachment.FindStringSubmatch(m)
		if strings.Contains(parts[1], "application/vnd.basecamp.mention") {
			if name := strings.TrimSpace(anyTag.ReplaceAllString(parts[2], "")); name != "" {
				return "@" + name
			}
		}
		return ""
	})
}
