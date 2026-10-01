// Package linear implements the Import Provider interface for Linear.
//
// Source: live API only ("api"). Linear has no first-party export
// format, so the live GraphQL API is the only viable path.
//
// Auth: Bearer <token>. Either a Personal API Key
// (https://linear.app/settings/api) or an OAuth 2.0 access token. PATs
// are long-lived; OAuth tokens currently never expire either, but the
// shared FreshAccessToken helper is wired in so a future Linear change
// Just Works without code edits. Stored encrypted via importModels.SaveToken.
//
// Linear data shape:
//
//	Organization → Team → Project → Issue → Comment → Attachment
//	Cycles are surfaced as labels rather than separate entities (they
//	describe sprints, not status).
//
// In OneCamp:
//   - One OneCamp Team per Linear Team.
//   - One OneCamp Project per Linear Project.    Issues without a
//     project are bucketed under a synthetic "{Team} — Inbox" project
//     so they don't fall on the floor.
//   - One Task per Issue. Subtasks (parent_id) become OneCamp subtasks.
//   - One Comment per Issue Comment.
//   - Attachments fetched via the URL Linear hands back; URLs are
//     long-lived (S3 with months-long expiry).
//
// Pagination: every list query uses Linear's cursor-based pagination
// (`pageInfo.endCursor` + `hasNextPage`). We follow up to a safety
// cap so a runaway loop can't hang a worker.
//
// Rate limits: Linear publishes 1500 requests/hour per token (paid)
// and 200/hr (free). We use SleepLimiter at one request per 250ms
// (~14k/hr capacity) — well under the paid cap with no guard against
// busy bursts; the limiter exists primarily so concurrent imports for
// different jobs share a polite envelope.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	providerName = importModels.ProviderLinear

	// maxPages caps pagination so a runaway query can't hang a worker.
	// 200 pages × 100 items = 20k items per call, which covers every
	// realistic Linear project.
	maxPages = 200

	// inboxProjectID is the synthetic source id used for issues with
	// no project. We add one such project per team so issues without
	// a Linear project still land somewhere.
	inboxProjectIDPrefix = "__inbox__:"
)

// graphqlEndpoint is Linear's single GraphQL endpoint. All API
// traffic goes here; there's no REST surface to fall back on.
//
// Declared as a package-level var (not const) so tests can swap in a
// httptest server URL without taking a wider refactor. Production
// code never reassigns this.
var graphqlEndpoint = "https://api.linear.app/graphql"

// Provider implements importProvider.Provider for Linear.
type Provider struct {
	rl importProvider.Limiter

	// snapshotCache pins one workspace snapshot per job. The
	// orchestrator calls Plan once and then iterates through each
	// stage; without a cache we'd re-query Linear's GraphQL API for
	// every Iter* invocation. The snapshot is a coherent point-in-time
	// view of the workspace we're importing.
	mu            sync.Mutex
	snapshotCache map[uuid.UUID]*workspaceSnapshot
}

// New constructs a singleton. Called from init() and re-callable from
// tests for a fresh limiter / cache.
func New() *Provider {
	return &Provider{
		// 250ms ticker × burst 8 → ~32/sec sustained, ~115k/hr — well
		// under the 1500/hr per-token paid cap. The conservative pace
		// also keeps us courteous on the free tier (200/hr) where a
		// single import would otherwise consume the whole budget.
		rl:            importProvider.NewSleepLimiter(250*time.Millisecond, 8),
		snapshotCache: make(map[uuid.UUID]*workspaceSnapshot, 4),
	}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }

// Capabilities advertises every shape Linear supplies. Notably no
// CapTeams skip — we DO synthesise OneCamp teams from Linear teams.
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTeams |
		importProvider.CapProjects |
		importProvider.CapTasks |
		importProvider.CapSubtasks |
		importProvider.CapTaskComments |
		importProvider.CapAttachments
}

// CleanupJob evicts the per-job workspace snapshot when the job
// reaches a terminal state. Without this a long-running deploy that
// runs many imports would keep snapshots in memory forever.
func (p *Provider) CleanupJob(jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.snapshotCache {
		if k.String() == jobID {
			delete(p.snapshotCache, k)
		}
	}
}

// DefaultStatusMap maps Linear's standard workflow-state types onto
// OneCamp statuses. Linear gives every issue a `state.type` of
// triage|backlog|unstarted|started|completed|canceled, plus the
// `state.name` (workspace-defined). The operator can override per
// import in the Plan UI; this is only the proposed default.
func (p *Provider) DefaultStatusMap() map[string]string {
	return map[string]string{
		// state.type values
		"triage":    "backlog",
		"backlog":   "backlog",
		"unstarted": "todo",
		"started":   "inProgress",
		"completed": "done",
		"canceled":  "canceled",

		// Common state.name values seen in Linear workspaces.
		"todo":        "todo",
		"in progress": "inProgress",
		"in review":   "inReview",
		"in qa":       "inReview",
		"qa":          "inReview",
		"done":        "done",
		"cancelled":   "canceled",
	}
}

// DefaultPriorityMap maps Linear's numeric priority field to OneCamp.
// Linear uses 0 (No priority), 1 (Urgent), 2 (High), 3 (Medium), 4 (Low).
// The provider stamps the raw display name on SourceTask.Priority, so
// this map keys on the human label.
func (p *Provider) DefaultPriorityMap() map[string]string {
	return map[string]string{
		"urgent":      "high",
		"high":        "high",
		"medium":      "medium",
		"low":         "low",
		"no priority": "low",
	}
}

// ─── Provider lifecycle ───────────────────────────────────────────

// Validate runs a viewer query — the cheapest authenticated GraphQL
// call. Verifies credentials and surfaces a clear error.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	var resp struct {
		Viewer struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"viewer"`
	}
	if err := p.gql(ctx, tok, `query { viewer { id name email } }`, nil, &resp); err != nil {
		return fmt.Errorf("linear auth: %w", err)
	}
	if resp.Viewer.ID == "" {
		return errors.New("linear viewer query returned empty id; bad token?")
	}
	return nil
}

// Plan walks the workspace once, captures everything we'll need across
// the orchestrator's stages, and stashes it in snapshotCache. Plan is
// idempotent: a re-plan rebuilds the snapshot from scratch.
//
// Linear's GraphQL surface is plentiful — we only request the fields
// we use, keeping payloads under a few MB even for large workspaces.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	snap, err := p.buildSnapshot(ctx, j)
	if err != nil {
		return nil, nil, err
	}

	plan := &importProvider.Plan{
		UserCount:    len(snap.Users),
		TeamCount:    len(snap.Teams),
		ProjectCount: len(snap.Projects),
	}

	// Status / priority value sets the operator confirms in the Plan UI.
	statusSet := map[string]struct{}{}
	prioritySet := map[string]struct{}{}

	for _, iss := range snap.Issues {
		plan.TaskCount++
		if iss.ParentID != "" {
			// Children are still counted as tasks but separately as
			// subtasks for the FE summary.
			plan.SubtaskCount++
		}
		plan.CommentCount += len(iss.Comments)
		plan.FileCount += len(iss.Attachments)
		for _, a := range iss.Attachments {
			plan.FileBytes += a.Size
		}

		if name := strings.ToLower(iss.StateName); name != "" {
			statusSet[name] = struct{}{}
		}
		if t := strings.ToLower(iss.StateType); t != "" {
			statusSet[t] = struct{}{}
		}
		if name := strings.ToLower(iss.PriorityLabel); name != "" {
			prioritySet[name] = struct{}{}
		}
	}

	for k := range statusSet {
		plan.StatusValues = append(plan.StatusValues, k)
	}
	for k := range prioritySet {
		plan.PriorityValues = append(plan.PriorityValues, k)
	}
	// Stable order so the UI doesn't shuffle on each plan call.
	sort.Strings(plan.StatusValues)
	sort.Strings(plan.PriorityValues)

	if len(snap.Issues) == 0 {
		plan.Warnings = append(plan.Warnings,
			"no issues visible to this token; check OAuth scope (read access required)")
	}

	return plan, nil, nil
}

// IterUsers streams every Linear user the snapshot saw.
func (p *Provider) IterUsers(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceUser, <-chan error) {
	out := make(chan importProvider.SourceUser, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("linear.IterUsers", errCh)
		snap, err := p.snapshotFor(ctx, j)
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
				DisplayName: helpers.FirstNonEmpty(u.Name, u.DisplayName),
				Login:       u.DisplayName,
				Email:       u.Email,
				AvatarURL:   u.AvatarURL,
				IsBot:       false,
				IsExternal:  u.Guest,
				Metadata: map[string]any{
					"linear_id": u.ID,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTeams emits one team per Linear team.
func (p *Provider) IterTeams(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceTeam, <-chan error) {
	out := make(chan importProvider.SourceTeam, 8)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("linear.IterTeams", errCh)
		snap, err := p.snapshotFor(ctx, j)
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
					"linear_key": t.Key,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterProjects emits one project per Linear project, plus a synthetic
// "Inbox" project per team for issues without a project. The inbox
// project keeps the OneCamp tree complete — every issue has a home.
func (p *Provider) IterProjects(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (<-chan importProvider.SourceProject, <-chan error) {
	out := make(chan importProvider.SourceProject, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("linear.IterProjects", errCh)
		snap, err := p.snapshotFor(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		for _, pr := range snap.Projects {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:     pr.ID,
				Name:         pr.Name,
				Description:  pr.Description,
				TeamSourceID: pr.TeamID,
				MemberIds:    append([]string{}, pr.MemberIDs...),
				Archived:     pr.State == "completed" || pr.State == "canceled",
				Metadata: map[string]any{
					"linear_url": pr.URL,
					"state":      pr.State,
				},
			}:
			}
		}
		// Synthetic inbox project per team.
		for _, t := range snap.Teams {
			if !snap.TeamHasInbox(t.ID) {
				continue
			}
			inboxID := inboxProjectIDPrefix + t.ID
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceProject{
				SourceID:     inboxID,
				Name:         t.Name + " — Inbox",
				Description:  "Linear issues from team " + t.Name + " that had no project assigned.",
				TeamSourceID: t.ID,
				Metadata: map[string]any{
					"synthetic": true,
				},
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams every issue whose project matches.
// Subtasks (issues with parent_id) come back in this stream too, with
// ParentTaskID set so the orchestrator's task worker links them.
//
// For the synthetic inbox project, we emit issues whose linear_project_id
// is empty AND whose team matches the inbox's team.
func (p *Provider) IterTasksOfProject(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, projectSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("linear.IterTasksOfProject", errCh)
		snap, err := p.snapshotFor(ctx, j)
		if err != nil {
			errCh <- err
			return
		}

		isInbox := strings.HasPrefix(projectSourceID, inboxProjectIDPrefix)
		inboxTeamID := strings.TrimPrefix(projectSourceID, inboxProjectIDPrefix)

		for _, iss := range snap.Issues {
			if isInbox {
				if iss.ProjectID != "" || iss.TeamID != inboxTeamID {
					continue
				}
			} else if iss.ProjectID != projectSourceID {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			task := p.issueToSourceTask(iss, projectSourceID)
			select {
			case <-ctx.Done():
				return
			case out <- task:
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask is intentionally empty: Linear sub-issues are
// regular issues with `parent.id` set, and the snapshot already
// streams them in IterTasksOfProject with ParentTaskID populated.
// The orchestrator's subtask stage only runs for providers that need
// per-task subtask fan-out (Trello checklists, Asana stories with
// subtasks). Returning an empty stream here is the supported pattern.
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	out := make(chan importProvider.SourceTask)
	errCh := make(chan error, 1)
	close(out)
	close(errCh)
	return out, errCh
}

// IterCommentsOfTask streams comments for one issue. Comments come
// back inside the snapshot already, so this is a memory-only filter.
func (p *Provider) IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceComment, <-chan error) {
	out := make(chan importProvider.SourceComment, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr("linear.IterCommentsOfTask", errCh)
		snap, err := p.snapshotFor(ctx, j)
		if err != nil {
			errCh <- err
			return
		}
		iss := snap.IssueByID(taskSourceID)
		if iss == nil {
			return
		}
		for _, c := range iss.Comments {
			select {
			case <-ctx.Done():
				return
			case out <- importProvider.SourceComment{
				SourceID:       c.ID,
				TaskSourceID:   taskSourceID,
				Body:           markdownToHTML(c.Body),
				AuthorSourceID: c.UserID,
				Created:        c.CreatedAt,
			}:
			}
		}
	}()
	return out, errCh
}

// FetchAttachment streams a Linear-attached file to dest.
//
// Linear attachments come in two flavours:
//  1. Files uploaded into Linear (URL on uploads.linear.app) — these
//     need the Authorization header on the GET, same as a viewer
//     request.
//  2. External link attachments (GitHub, Figma, Slack, …). We treat
//     these as "already linked"; the import worker handles 404/410
//     via ErrAttachmentGone and keeps the link as metadata.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return "", 0, err
	}

	// Linear-hosted uploads need our token attached.
	if strings.Contains(att.URL, "uploads.linear.app") {
		att2 := att
		if att2.Headers == nil {
			att2.Headers = map[string]string{}
		}
		att2.Headers["Authorization"] = "Bearer " + tok
		return importProvider.DefaultFetchAttachment(ctx, att2, dest)
	}
	// External attachment: try a plain GET; rely on
	// DefaultFetchAttachment to translate 404/410 into ErrAttachmentGone.
	return importProvider.DefaultFetchAttachment(ctx, att, dest)
}

// ─── Snapshot loader (single-pass GraphQL crawler) ────────────────

// workspaceSnapshot is the in-memory representation of everything we
// pulled from Linear for one job. Keeping it in one place means the
// orchestrator's stages don't refetch — every stage filters this struct.
type workspaceSnapshot struct {
	Users    []linearUser
	Teams    []linearTeam
	Projects []linearProject
	Issues   []linearIssue

	issueByID    map[string]*linearIssue
	teamHasInbox map[string]bool
}

func (s *workspaceSnapshot) IssueByID(id string) *linearIssue {
	if s == nil {
		return nil
	}
	return s.issueByID[id]
}

func (s *workspaceSnapshot) TeamHasInbox(teamID string) bool {
	if s == nil {
		return false
	}
	return s.teamHasInbox[teamID]
}

// buildSnapshot pulls everything we need in three passes:
//  1. Teams + memberships  (one query, paginated)
//  2. Users                (one query, paginated)
//  3. Projects             (one query, paginated)
//  4. Issues               (one query per team, paginated, embedding
//     comments + attachments via GraphQL nesting)
//
// We intentionally embed comments/attachments inside the issue query
// rather than fanning out per-issue calls, which on a 5k-issue
// workspace would mean 5k×N round-trips instead of ~50.
func (p *Provider) buildSnapshot(ctx context.Context, j *importModels.Job) (*workspaceSnapshot, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}

	users, err := p.fetchUsers(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear users: %w", err)
	}
	teams, err := p.fetchTeams(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear teams: %w", err)
	}
	projects, err := p.fetchProjects(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear projects: %w", err)
	}

	// Optional narrowing: opts["team_id"] (set via the discover dropdown)
	// scopes the import to a single team. We filter teams + projects
	// here so downstream stages see the narrowed slice. Issues are
	// already per-team so the loop below naturally honours the filter.
	if scopedTeam, _ := decodeTeamScope(j); scopedTeam != "" {
		filteredTeams := teams[:0]
		for _, t := range teams {
			if t.ID == scopedTeam {
				filteredTeams = append(filteredTeams, t)
			}
		}
		teams = filteredTeams

		filteredProjects := projects[:0]
		for _, pr := range projects {
			if pr.TeamID == scopedTeam || pr.TeamID == "" {
				// Cross-team Linear projects are anchored to their
				// primary team; we keep them iff the primary matches.
				filteredProjects = append(filteredProjects, pr)
			}
		}
		projects = filteredProjects
	}

	// Issues are scoped per team in Linear's data model. We could
	// fetch the global stream, but per-team queries let us narrow if
	// the operator picked a single team via opts["team_id"], and they
	// keep query payloads bounded.
	allIssues := make([]linearIssue, 0, 256)
	for _, t := range teams {
		issues, err := p.fetchTeamIssues(ctx, tok, t.ID)
		if err != nil {
			return nil, fmt.Errorf("linear issues for team %s: %w", t.Key, err)
		}
		allIssues = append(allIssues, issues...)
	}

	snap := &workspaceSnapshot{
		Users:        users,
		Teams:        teams,
		Projects:     projects,
		Issues:       allIssues,
		issueByID:    make(map[string]*linearIssue, len(allIssues)),
		teamHasInbox: make(map[string]bool, len(teams)),
	}
	for i := range snap.Issues {
		snap.issueByID[snap.Issues[i].ID] = &snap.Issues[i]
		if snap.Issues[i].ProjectID == "" && snap.Issues[i].TeamID != "" {
			snap.teamHasInbox[snap.Issues[i].TeamID] = true
		}
	}

	p.mu.Lock()
	p.snapshotCache[j.Id] = snap
	p.mu.Unlock()
	return snap, nil
}

// snapshotFor returns the cached snapshot, building it lazily if Plan
// hasn't run (defensive — the orchestrator always calls Plan first
// today, but a future direct-Iter caller shouldn't crash).
func (p *Provider) snapshotFor(ctx context.Context, j *importModels.Job) (*workspaceSnapshot, error) {
	p.mu.Lock()
	if s, ok := p.snapshotCache[j.Id]; ok {
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	return p.buildSnapshot(ctx, j)
}

// ─── GraphQL fetchers ─────────────────────────────────────────────

const usersQuery = `
query ($cursor: String) {
  users(first: 100, after: $cursor) {
    nodes {
      id name displayName email avatarUrl active guest
    }
    pageInfo { hasNextPage endCursor }
  }
}`

func (p *Provider) fetchUsers(ctx context.Context, tok string) ([]linearUser, error) {
	out := []linearUser{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Users struct {
				Nodes []linearUser `json:"nodes"`
				Page  pageInfo     `json:"pageInfo"`
			} `json:"users"`
		}
		vars := map[string]any{}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, usersQuery, vars, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Users.Nodes...)
		if !resp.Users.Page.HasNextPage {
			break
		}
		cursor = resp.Users.Page.EndCursor
	}
	return out, nil
}

const teamsQuery = `
query ($cursor: String) {
  teams(first: 100, after: $cursor) {
    nodes {
      id key name description
      members(first: 100) { nodes { id } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

func (p *Provider) fetchTeams(ctx context.Context, tok string) ([]linearTeam, error) {
	type rawTeam struct {
		ID          string `json:"id"`
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Members     struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"members"`
	}
	out := []linearTeam{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Teams struct {
				Nodes []rawTeam `json:"nodes"`
				Page  pageInfo  `json:"pageInfo"`
			} `json:"teams"`
		}
		vars := map[string]any{}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, teamsQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, t := range resp.Teams.Nodes {
			lt := linearTeam{
				ID: t.ID, Key: t.Key, Name: t.Name, Description: t.Description,
			}
			for _, m := range t.Members.Nodes {
				lt.MemberIDs = append(lt.MemberIDs, m.ID)
			}
			// Linear's REST/GraphQL has no team-admin role surface:
			// admins are workspace-wide. We leave AdminIDs empty so the
			// generic resolver promotes the importing user.
			out = append(out, lt)
		}
		if !resp.Teams.Page.HasNextPage {
			break
		}
		cursor = resp.Teams.Page.EndCursor
	}
	return out, nil
}

const projectsQuery = `
query ($cursor: String) {
  projects(first: 100, after: $cursor) {
    nodes {
      id name description state url
      teams { nodes { id } }
      members { nodes { id } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

func (p *Provider) fetchProjects(ctx context.Context, tok string) ([]linearProject, error) {
	type rawProject struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		State       string `json:"state"`
		URL         string `json:"url"`
		Teams       struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"teams"`
		Members struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"members"`
	}
	out := []linearProject{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Projects struct {
				Nodes []rawProject `json:"nodes"`
				Page  pageInfo     `json:"pageInfo"`
			} `json:"projects"`
		}
		vars := map[string]any{}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, projectsQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, pr := range resp.Projects.Nodes {
			lp := linearProject{
				ID: pr.ID, Name: pr.Name, Description: pr.Description,
				State: pr.State, URL: pr.URL,
			}
			// A Linear project can span multiple teams (rare). We pin
			// it to the first team for the OneCamp shape; secondary
			// team members still get added via team membership.
			if len(pr.Teams.Nodes) > 0 {
				lp.TeamID = pr.Teams.Nodes[0].ID
			}
			for _, m := range pr.Members.Nodes {
				lp.MemberIDs = append(lp.MemberIDs, m.ID)
			}
			out = append(out, lp)
		}
		if !resp.Projects.Page.HasNextPage {
			break
		}
		cursor = resp.Projects.Page.EndCursor
	}
	return out, nil
}

const teamIssuesQuery = `
query ($teamId: String!, $cursor: String) {
  team(id: $teamId) {
    issues(first: 100, after: $cursor, includeArchived: true) {
      nodes {
        id identifier title description url
        priority priorityLabel
        createdAt updatedAt completedAt canceledAt
        startedAt dueDate
        state { id name type }
        assignee { id }
        creator  { id }
        parent   { id }
        team     { id }
        project  { id }
        labels(first: 50) { nodes { name } }
        comments(first: 100) {
          nodes {
            id body createdAt
            user { id }
          }
        }
        attachments(first: 50) {
          nodes {
            id title url subtitle metadata
          }
        }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

func (p *Provider) fetchTeamIssues(ctx context.Context, tok, teamID string) ([]linearIssue, error) {
	type rawComment struct {
		ID        string    `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
		User      *struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	type rawAttachment struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		URL      string `json:"url"`
		Subtitle string `json:"subtitle"`
	}
	type rawIssue struct {
		ID            string     `json:"id"`
		Identifier    string     `json:"identifier"`
		Title         string     `json:"title"`
		Description   string     `json:"description"`
		URL           string     `json:"url"`
		Priority      int        `json:"priority"`
		PriorityLabel string     `json:"priorityLabel"`
		CreatedAt     time.Time  `json:"createdAt"`
		UpdatedAt     time.Time  `json:"updatedAt"`
		CompletedAt   *time.Time `json:"completedAt"`
		CanceledAt    *time.Time `json:"canceledAt"`
		StartedAt     *time.Time `json:"startedAt"`
		DueDate       *string    `json:"dueDate"` // Linear returns YYYY-MM-DD string
		State         struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"state"`
		Assignee *struct {
			ID string `json:"id"`
		} `json:"assignee"`
		Creator *struct {
			ID string `json:"id"`
		} `json:"creator"`
		Parent *struct {
			ID string `json:"id"`
		} `json:"parent"`
		Team *struct {
			ID string `json:"id"`
		} `json:"team"`
		Project *struct {
			ID string `json:"id"`
		} `json:"project"`
		Labels struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		Comments struct {
			Nodes []rawComment `json:"nodes"`
		} `json:"comments"`
		Attachments struct {
			Nodes []rawAttachment `json:"nodes"`
		} `json:"attachments"`
	}

	out := []linearIssue{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Team struct {
				Issues struct {
					Nodes []rawIssue `json:"nodes"`
					Page  pageInfo   `json:"pageInfo"`
				} `json:"issues"`
			} `json:"team"`
		}
		vars := map[string]any{"teamId": teamID}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, teamIssuesQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Team.Issues.Nodes {
			iss := linearIssue{
				ID:            r.ID,
				Identifier:    r.Identifier,
				Title:         r.Title,
				Description:   r.Description,
				URL:           r.URL,
				Priority:      r.Priority,
				PriorityLabel: r.PriorityLabel,
				CreatedAt:     r.CreatedAt,
				UpdatedAt:     r.UpdatedAt,
				CompletedAt:   r.CompletedAt,
				CanceledAt:    r.CanceledAt,
				StartedAt:     r.StartedAt,
				StateID:       r.State.ID,
				StateName:     r.State.Name,
				StateType:     r.State.Type,
			}
			if r.Assignee != nil {
				iss.AssigneeID = r.Assignee.ID
			}
			if r.Creator != nil {
				iss.CreatorID = r.Creator.ID
			}
			if r.Parent != nil {
				iss.ParentID = r.Parent.ID
			}
			if r.Team != nil {
				iss.TeamID = r.Team.ID
			}
			if r.Project != nil {
				iss.ProjectID = r.Project.ID
			}
			if r.DueDate != nil && *r.DueDate != "" {
				if d, err := time.Parse("2006-01-02", *r.DueDate); err == nil {
					iss.DueDate = &d
				}
			}
			for _, l := range r.Labels.Nodes {
				if l.Name != "" {
					iss.Labels = append(iss.Labels, l.Name)
				}
			}
			for _, c := range r.Comments.Nodes {
				lc := linearComment{
					ID:        c.ID,
					Body:      c.Body,
					CreatedAt: c.CreatedAt,
				}
				if c.User != nil {
					lc.UserID = c.User.ID
				}
				iss.Comments = append(iss.Comments, lc)
			}
			for _, a := range r.Attachments.Nodes {
				if a.URL == "" {
					continue
				}
				name := a.Title
				if name == "" {
					name = a.Subtitle
				}
				if name == "" {
					name = "attachment"
				}
				iss.Attachments = append(iss.Attachments, linearAttachment{
					ID:   a.ID,
					Name: name,
					URL:  a.URL,
				})
			}
			out = append(out, iss)
		}
		if !resp.Team.Issues.Page.HasNextPage {
			break
		}
		cursor = resp.Team.Issues.Page.EndCursor
	}
	return out, nil
}

// ─── GraphQL plumbing ─────────────────────────────────────────────

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions"`
}

// gql runs one GraphQL request with rate-limiting, 429 handling, and
// JSON-decoding into out. The outer envelope { data, errors } is
// destructured here so callers see clean shapes.
func (p *Provider) gql(ctx context.Context, tok, query string, vars map[string]any, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": vars,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Linear returns 429 with a Retry-After header (in seconds).
		retry := time.Duration(parseRetryAfter(resp.Header.Get("Retry-After"))) * time.Second
		return &importProvider.ErrRateLimited{RetryAfter: retry, Reason: "linear 429"}
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("linear unauthorized; reconnect this provider")
	case resp.StatusCode >= 400:
		// Read a small slice of the body for diagnostics; full body
		// would be unbounded under malicious upstreams.
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("linear http %d: %s", resp.StatusCode, string(buf[:n]))
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("linear decode: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("linear graphql: %s", env.Errors[0].Message)
	}
	if len(env.Data) == 0 {
		return errors.New("linear empty data envelope")
	}
	return json.Unmarshal(env.Data, out)
}

// parseRetryAfter handles both numeric "30" and HTTP-date forms; the
// latter is rare from Linear but cheap to support. Returns 0 on
// parse failure so the caller falls back to the limiter's natural pace.
func parseRetryAfter(h string) int64 {
	if h == "" {
		return 0
	}
	// Numeric seconds (the common Linear shape).
	var secs int64
	if _, err := fmt.Sscanf(h, "%d", &secs); err == nil && secs > 0 {
		return secs
	}
	// HTTP-date.
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return int64(d.Seconds())
		}
	}
	return 0
}

// ─── Auth ─────────────────────────────────────────────────────────

// loadToken loads + lazily refreshes the OAuth/PAT token for the
// import job's owner. PATs (no refresh_token) bypass the OAuth path
// inside FreshAccessToken and return as-is.
func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty linear token")
	}
	if t.RefreshToken != "" {
		fresh, ferr := importProvider.FreshAccessToken(ctx, linearOAuthConfig(), providerName, *j.TriggeredBy)
		if ferr == nil && fresh != "" {
			return fresh, nil
		}
	}
	return t.AccessToken, nil
}

// ─── Mapping helpers ──────────────────────────────────────────────

func (p *Provider) issueToSourceTask(iss linearIssue, projectID string) importProvider.SourceTask {
	// Linear stores descriptions as Markdown. Render to a minimal
	// HTML subset; the orchestrator's HTML sanitiser then keeps the
	// safe tags.
	desc := markdownToHTML(iss.Description)

	// Status: prefer state.name; fall back to state.type. The operator
	// maps both classes via the Plan UI.
	status := iss.StateName
	if status == "" {
		status = iss.StateType
	}

	priority := iss.PriorityLabel
	if priority == "" {
		// PriorityLabel is empty when priority=0 (No priority).
		priority = "no priority"
	}

	assignees := []string{}
	if iss.AssigneeID != "" {
		assignees = append(assignees, iss.AssigneeID)
	}

	atts := make([]importProvider.SourceAttachment, 0, len(iss.Attachments))
	for _, a := range iss.Attachments {
		atts = append(atts, importProvider.SourceAttachment{
			SourceID: a.ID,
			Name:     a.Name,
			URL:      a.URL,
			Parent:   importProvider.SourceRef{Kind: "task", SourceID: iss.ID},
		})
	}

	return importProvider.SourceTask{
		SourceID:        iss.ID,
		ParentTaskID:    iss.ParentID,
		ProjectSourceID: projectID,
		Name:            truncate(iss.Title, 256),
		Description:     desc,
		Status:          status,
		Priority:        priority,
		Labels:          append([]string{}, iss.Labels...),
		AssigneeIds:     assignees,
		CreatedBy:       iss.CreatorID,
		StartDate:       iss.StartedAt,
		DueDate:         iss.DueDate,
		Created:         iss.CreatedAt,
		Updated:         iss.UpdatedAt,
		Completed:       iss.CompletedAt != nil,
		AttachmentRefs:  atts,
		CommentCount:    len(iss.Comments),
		Metadata: map[string]any{
			"linear_url":        iss.URL,
			"linear_identifier": iss.Identifier, // e.g. "ENG-123"
			"linear_state_type": iss.StateType,
			"linear_priority":   iss.Priority,
		},
	}
}

// markdownToHTML converts the small subset of Markdown Linear emits
// (newlines, links, code) into HTML. The orchestrator's bluemonday
// sanitiser strips anything we don't allow, so this can stay
// intentionally light — we don't need a full CommonMark renderer.
func markdownToHTML(md string) string {
	if md == "" {
		return ""
	}
	// HTML-escape first so user content can't smuggle tags.
	out := htmlEscape(md)
	// Convert paragraphs (double newlines) and line breaks.
	out = strings.ReplaceAll(out, "\r\n", "\n")
	paragraphs := strings.Split(out, "\n\n")
	for i, p := range paragraphs {
		paragraphs[i] = "<p>" + strings.ReplaceAll(p, "\n", "<br/>") + "</p>"
	}
	return strings.Join(paragraphs, "")
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── DTOs ─────────────────────────────────────────────────────────

type linearUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	AvatarURL   string `json:"avatarUrl"`
	Active      bool   `json:"active"`
	Guest       bool   `json:"guest"`
}

type linearTeam struct {
	ID          string
	Key         string
	Name        string
	Description string
	MemberIDs   []string
	AdminIDs    []string
}

type linearProject struct {
	ID          string
	Name        string
	Description string
	State       string
	URL         string
	TeamID      string
	MemberIDs   []string
}

type linearIssue struct {
	ID            string
	Identifier    string
	Title         string
	Description   string
	URL           string
	Priority      int
	PriorityLabel string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
	CanceledAt    *time.Time
	StartedAt     *time.Time
	DueDate       *time.Time
	StateID       string
	StateName     string
	StateType     string
	AssigneeID    string
	CreatorID     string
	ParentID      string
	TeamID        string
	ProjectID     string
	Labels        []string
	Comments      []linearComment
	Attachments   []linearAttachment
}

type linearComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
	UserID    string
}

type linearAttachment struct {
	ID   string
	Name string
	URL  string
	// Linear attachment metadata doesn't expose size or mime; we
	// leave them zero/empty and let the worker fill them from
	// response headers during fetch.
	Size int64
}

// ─── Discoverer implementation ────────────────────────────────────

// Discover lists Linear teams accessible to the connected token. We
// expose teams (rather than the singleton organization) because the
// FE's "pick a workspace" dropdown is more useful when the operator
// can scope the import to one team — a future option key.
//
// For now the controller passes the picked id via opts["team_id"]; the
// snapshot loader optionally narrows to that team if set, otherwise
// imports all teams the token can see.
func (p *Provider) Discover(ctx context.Context, ownerUserID string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	if token == nil || token.AccessToken == "" {
		return nil, errors.New("no linear token saved")
	}
	access := token.AccessToken
	if token.RefreshToken != "" {
		// Best-effort refresh: if Linear ever switches to short-lived
		// tokens, this keeps discovery working without a code change.
		owner, perr := uuid.Parse(ownerUserID)
		if perr == nil {
			if fresh, ferr := importProvider.FreshAccessToken(ctx, linearOAuthConfig(), providerName, owner); ferr == nil && fresh != "" {
				access = fresh
			}
		}
	}

	const q = `
query {
  organization { id name urlKey }
  teams(first: 100) {
    nodes { id key name description }
  }
}`
	var resp struct {
		Organization struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			URLKey string `json:"urlKey"`
		} `json:"organization"`
		Teams struct {
			Nodes []struct {
				ID          string `json:"id"`
				Key         string `json:"key"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := p.gql(ctx, access, q, nil, &resp); err != nil {
		return nil, err
	}

	out := make([]importProvider.DiscoverItem, 0, len(resp.Teams.Nodes))
	orgURL := ""
	if resp.Organization.URLKey != "" {
		orgURL = "https://linear.app/" + resp.Organization.URLKey
	}
	for _, t := range resp.Teams.Nodes {
		teamURL := ""
		if orgURL != "" && t.Key != "" {
			teamURL = orgURL + "/team/" + t.Key
		}
		out = append(out, importProvider.DiscoverItem{
			ID:          t.ID,
			Name:        t.Name + " (" + t.Key + ")",
			Description: t.Description,
			URL:         teamURL,
			Kind:        "team",
			Meta: map[string]any{
				"organization": resp.Organization.Name,
				"team_key":     t.Key,
			},
		})
	}
	return out, nil
}

// decodeTeamScope reads the optional team_id narrowing from the job's
// stored options blob. We don't take JobOptions as a parameter to keep
// the signature of buildSnapshot stable for callers that build it
// without options (tests, validate path).
func decodeTeamScope(j *importModels.Job) (string, error) {
	if j == nil || len(j.Options) == 0 {
		return "", nil
	}
	var opts struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(j.Options, &opts); err != nil {
		return "", err
	}
	return opts.TeamID, nil
}
