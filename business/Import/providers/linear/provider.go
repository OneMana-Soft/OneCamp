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
	maxPages = 400

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

// statusRules reads Linear's answers. Only a 401 refuses the token; its
// other errors come in a 200's body. Its limits are per hour.
var statusRules = importProvider.StatusRules{Name: "Linear", Wait: time.Minute, OnlyUnauthorized: true}

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
		"urgent":      "urgent",
		"high":        "high",
		"medium":      "medium",
		"low":         "low",
		"no priority": "",
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
	if err := p.gql(ctx, tok, viewerQuery, nil, &resp); err != nil {
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
	return snap.plan(), nil, nil
}

// plan is what the snapshot will bring, for the person running the import to
// confirm: its counts, a project's milestones among the tasks, and the
// statuses and priorities to map.
func (s *workspaceSnapshot) plan() *importProvider.Plan {
	plan := &importProvider.Plan{
		UserCount:    len(s.Users),
		TeamCount:    len(s.Teams),
		ProjectCount: len(s.Projects),
	}

	// Status / priority value sets the operator confirms in the Plan UI.
	plan.StatusValues, plan.StatusKinds = planStatuses(s.Issues)
	prioritySet := map[string]struct{}{}

	for _, pr := range s.Projects {
		plan.TaskCount += len(s.milestones[pr.ID])
	}
	for _, iss := range s.Issues {
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

		if name := strings.ToLower(iss.PriorityLabel); name != "" {
			prioritySet[name] = struct{}{}
		}
	}

	for k := range prioritySet {
		plan.PriorityValues = append(plan.PriorityValues, k)
	}
	// Stable order so the UI doesn't shuffle on each plan call.
	sort.Strings(plan.PriorityValues)

	if len(s.Issues) == 0 {
		plan.Warnings = append(plan.Warnings,
			"no issues visible to this token; check OAuth scope (read access required)")
	}
	return plan
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
			case out <- u.source():
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
			issues := snap.projectIssues(pr.ID)
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
				HighestNumber: highestNumber(issues),
				Fields:        milestoneFields(snap.milestones[pr.ID]),
				Cycles:        projectCycles(issues, snap.cycleByID),
				CycleSchedule: snap.schedules[pr.TeamID],
				EstimateScale: snap.scales[pr.TeamID],
			}:
			}
		}
		// Synthetic inbox project per team.
		for _, t := range snap.Teams {
			if !snap.TeamHasInbox(t.ID) {
				continue
			}
			inboxID := inboxProjectIDPrefix + t.ID
			issues := snap.projectIssues(inboxID)
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
				HighestNumber: highestNumber(issues),
				Cycles:        projectCycles(issues, snap.cycleByID),
				CycleSchedule: snap.schedules[t.ID],
				EstimateScale: snap.scales[t.ID],
			}:
			}
		}
	}()
	return out, errCh
}

// IterTasksOfProject streams every issue whose project matches.
// Subtasks (issues with parent_id) come back in this stream too, with
// ParentTaskID set so the orchestrator's task worker links them. The
// project's milestones follow its issues, each a Milestone task.
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

		for _, iss := range snap.projectIssues(projectSourceID) {
			if ctx.Err() != nil {
				return
			}
			task := snap.task(p, *iss, projectSourceID)
			select {
			case <-ctx.Done():
				return
			case out <- task:
			}
		}
		for _, m := range snap.milestones[projectSourceID] {
			select {
			case <-ctx.Done():
				return
			case out <- m.task(projectSourceID):
			}
		}
	}()
	return out, errCh
}

// IterSubtasksOfTask streams an issue's sub-issues. They are regular issues
// with `parent.id` set: IterTasksOfProject sends them with ParentTaskID, the
// task worker schedules a subtask chunk for the parent instead of importing
// them, and that chunk reads them here. (It used to be empty, and every
// sub-issue was dropped.)
func (p *Provider) IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, taskSourceID string) (<-chan importProvider.SourceTask, <-chan error) {
	// The task worker turns an issue with a parent into a subtask chunk for
	// that parent and asks for its sub-issues here; the snapshot has them.
	// A sub-issue belongs to its own project, or to its team's inbox.
	var snap *workspaceSnapshot
	return importProvider.StreamTasks(ctx, "linear.IterSubtasksOfTask",
		func() ([]linearIssue, error) {
			var err error
			if snap, err = p.snapshotFor(ctx, j); err != nil {
				return nil, err
			}
			return snap.Issues, nil
		},
		func(iss linearIssue) bool { return iss.ParentID == taskSourceID },
		func(iss linearIssue) importProvider.SourceTask {
			project := iss.ProjectID
			if project == "" {
				project = inboxProjectIDPrefix + iss.TeamID
			}
			return snap.task(p, iss, project)
		})
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
	// External attachments get a plain GET; DefaultFetchAttachment turns
	// 404/410 into ErrAttachmentGone.
	return importProvider.DefaultFetchAttachment(ctx, linearDownload(att, tok), dest)
}

// linearUploadHost serves files uploaded into Linear, which need the
// importing admin's token. Nothing else may be sent it: a link attachment's
// URL is whatever anyone in the Linear workspace typed, and the check used to
// be a substring search, so "https://evil.example/?u=uploads.linear.app" got
// the token.
const linearUploadHost = "uploads.linear.app"

// linearDownload is att as it is fetched: with the token when it is a Linear
// upload, and without one when it isn't.
func linearDownload(att importProvider.SourceAttachment, tok string) importProvider.SourceAttachment {
	return importProvider.WithAuthorization(att, "Bearer "+tok, linearUploadHost)
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
	cycleByID    map[string]linearCycle
	schedules    map[string]*importProvider.SourceCycleSchedule // by team
	scales       map[string]*importProvider.SourceEstimateScale // by team
	milestones   map[string][]linearMilestone                   // by project, in Linear's order
	keptTeams    map[string]keptTeam                            // by team, those Linear doesn't show everyone
}

func (s *workspaceSnapshot) IssueByID(id string) *linearIssue {
	if s == nil {
		return nil
	}
	return s.issueByID[id]
}

// projectIssues are the issues a project here holds: its own, or for a
// team's inbox, the team's issues in no project.
func (s *workspaceSnapshot) projectIssues(projectSourceID string) []*linearIssue {
	inboxTeam, isInbox := strings.CutPrefix(projectSourceID, inboxProjectIDPrefix)
	var out []*linearIssue
	for i := range s.Issues {
		iss := &s.Issues[i]
		if (isInbox && iss.ProjectID == "" && iss.TeamID == inboxTeam) || (!isInbox && iss.ProjectID == projectSourceID) {
			out = append(out, iss)
		}
	}
	return out
}

// task is iss as a task of projectID, kept to its team's members when Linear
// doesn't show its team to everyone (SourceTask.Restricted).
func (s *workspaceSnapshot) task(p *Provider, iss linearIssue, projectID string) importProvider.SourceTask {
	return keptToTeam(p.issueToSourceTask(iss, projectID), s.keptTeams, iss.TeamID)
}

// keptToTeam is st kept to its team's members when Linear doesn't show its
// team (teamID) to everyone (kept, keptTeams'): the import's rule, and a
// re-check's (recheck.go). Pure.
func keptToTeam(st importProvider.SourceTask, kept map[string]keptTeam, teamID string) importProvider.SourceTask {
	if k, ok := kept[teamID]; ok {
		st.Restricted, st.Audience = k.why, k.members
	}
	return st
}

// keptTeam is a team Linear doesn't show everyone: why, in the report's
// words, and its members, who see its issues.
type keptTeam struct {
	why     string
	members []string
}

// keptTeams are the teams whose issues Linear shows to some people only: a
// private team's to its members, and a restricted team's (a team inside a
// private one) to that private team's, of whom only its own members are
// known here. A visibility this doesn't know is kept too. Their members are
// those Linear lists first (teamsQuery's hundred): any more are left for the
// admin to share with. Pure.
func keptTeams(teams []linearTeam) map[string]keptTeam {
	out := map[string]keptTeam{}
	for _, t := range teams {
		why := "a team that isn't public"
		switch t.Visibility {
		case "public":
			continue
		case "private":
			why = "a private team"
		case "restricted":
			why = "a team inside a private team"
		}
		out[t.ID] = keptTeam{why: why, members: t.MemberIDs}
	}
	return out
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
	labels, err := p.fetchLabels(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear labels: %w", err)
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
	cycles, err := p.fetchCycles(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear cycles: %w", err)
	}
	cycleByID := make(map[string]linearCycle, len(cycles))
	teamCycles := map[string][]linearCycle{}
	for _, c := range cycles {
		cycleByID[c.ID] = c
		teamCycles[c.Team.ID] = append(teamCycles[c.Team.ID], c)
	}
	schedules := make(map[string]*importProvider.SourceCycleSchedule, len(teams))
	for _, t := range teams {
		schedules[t.ID] = cycleSchedule(t, teamCycles[t.ID], time.Now())
	}
	milestones, err := p.fetchMilestones(ctx, tok)
	if err != nil {
		return nil, fmt.Errorf("linear milestones: %w", err)
	}
	milestoneByID := make(map[string]linearMilestone, len(milestones))
	for _, m := range milestones {
		milestoneByID[m.ID] = m
	}

	snap := &workspaceSnapshot{
		Users:        users,
		Teams:        teams,
		Projects:     projects,
		Issues:       allIssues,
		issueByID:    make(map[string]*linearIssue, len(allIssues)),
		teamHasInbox: make(map[string]bool, len(teams)),
		cycleByID:    cycleByID,
		schedules:    schedules,
		scales:       estimateScales(teams),
		milestones:   milestonesByProject(milestones),
		keptTeams:    keptTeams(teams),
	}
	for i := range snap.Issues {
		placeLabels(&snap.Issues[i], labels)
		if m, ok := milestoneByID[snap.Issues[i].MilestoneID]; ok {
			snap.Issues[i].Milestone = &m
		}
		snap.issueByID[snap.Issues[i].ID] = &snap.Issues[i]
		if snap.Issues[i].ProjectID == "" && snap.Issues[i].TeamID != "" {
			snap.teamHasInbox[snap.Issues[i].TeamID] = true
		}
	}
	linkRelations(snap.Issues, snap.issueByID)

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

// viewerQuery is the cheapest authenticated read: whose token this is.
const viewerQuery = `query { viewer { id name email } }`

// discoverQuery is what the token can see: its workspace and teams.
const discoverQuery = `
query {
  organization { id name urlKey }
  teams(first: 100) {
    nodes { id key name description }
  }
}`

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
	return rootList[linearUser](ctx, p, tok, usersQuery, "users")
}

const teamsQuery = `
query ($cursor: String) {
  teams(first: 50, after: $cursor) {
    nodes {
      id key name description visibility
      cyclesEnabled cycleDuration cycleCooldownTime upcomingCycleCount
      issueEstimationType issueEstimationExtended issueEstimationAllowZero defaultIssueEstimate
      inheritIssueEstimation parent { id }
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
		Visibility  string `json:"visibility"`
		// Cycle settings, in weeks.
		CyclesEnabled bool    `json:"cyclesEnabled"`
		CycleWeeks    float64 `json:"cycleDuration"`
		CooldownWeeks float64 `json:"cycleCooldownTime"`
		Upcoming      float64 `json:"upcomingCycleCount"`
		// Estimate settings (estimates.go).
		EstimationType      string  `json:"issueEstimationType"`
		EstimationExtended  bool    `json:"issueEstimationExtended"`
		EstimationAllowZero bool    `json:"issueEstimationAllowZero"`
		DefaultEstimate     float64 `json:"defaultIssueEstimate"`
		InheritEstimation   bool    `json:"inheritIssueEstimation"`
		Parent              *struct {
			ID string `json:"id"`
		} `json:"parent"`
		Members struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"members"`
	}
	raw, err := rootList[rawTeam](ctx, p, tok, teamsQuery, "teams")
	if err != nil {
		return nil, err
	}
	out := make([]linearTeam, 0, len(raw))
	for _, t := range raw {
		lt := linearTeam{
			ID: t.ID, Key: t.Key, Name: t.Name, Description: t.Description, Visibility: t.Visibility,
			CyclesEnabled: t.CyclesEnabled, CycleWeeks: int(t.CycleWeeks), CooldownWeeks: int(t.CooldownWeeks), Upcoming: int(t.Upcoming),
			EstimationType: t.EstimationType, EstimationExtended: t.EstimationExtended, EstimationAllowZero: t.EstimationAllowZero,
			DefaultEstimate: t.DefaultEstimate, InheritEstimation: t.InheritEstimation,
		}
		if t.Parent != nil {
			lt.ParentID = t.Parent.ID
		}
		for _, m := range t.Members.Nodes {
			lt.MemberIDs = append(lt.MemberIDs, m.ID)
		}
		// Linear's REST/GraphQL has no team-admin role surface:
		// admins are workspace-wide. We leave AdminIDs empty so the
		// generic resolver promotes the importing user.
		out = append(out, lt)
	}
	return out, nil
}

const projectsQuery = `
query ($cursor: String) {
  projects(first: 50, after: $cursor) {
    nodes {
      id name description url
      status { type }
      teams(first: 20) { nodes { id } }
      members(first: 50) { nodes { id } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

func (p *Provider) fetchProjects(ctx context.Context, tok string) ([]linearProject, error) {
	type rawProject struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		// status is the project's ProjectStatus; Project.state is deprecated
		// (schema_test.go keeps the import off deprecated fields).
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
		URL   string `json:"url"`
		Teams struct {
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
	raw, err := rootList[rawProject](ctx, p, tok, projectsQuery, "projects")
	if err != nil {
		return nil, err
	}
	out := make([]linearProject, 0, len(raw))
	for _, pr := range raw {
		lp := linearProject{
			ID: pr.ID, Name: pr.Name, Description: pr.Description,
			State: pr.Status.Type, URL: pr.URL,
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
	return out, nil
}

// teamIssuesQuery stays under Linear's 10,000-point complexity cap: each
// connection costs its page size times what it asks for, so 100 issues with
// 100 comments each (as it was) was refused outright.
const teamIssuesQuery = `
query ($teamId: String!, $cursor: String) {
  team(id: $teamId) {
    issues(first: 50, after: $cursor, includeArchived: true) {
      nodes {
        id identifier previousIdentifiers title description url
        priority priorityLabel
        createdAt updatedAt completedAt canceledAt
        startedAt dueDate
        state { id name type }
        assignee { id }
        creator  { id }
        parent   { id }
        team     { id }
        project  { id }
        cycle    { id }
        projectMilestone { id }
        estimate
        labelIds
        inverseRelations(first: 10) {
          nodes { type issue { id identifier url } }
          pageInfo { hasNextPage endCursor }
        }
        comments(first: 20) {
          nodes { id body createdAt user { id } }
          pageInfo { hasNextPage endCursor }
        }
        attachments(first: 10) {
          nodes { id title url subtitle }
          pageInfo { hasNextPage endCursor }
        }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

func (p *Provider) fetchTeamIssues(ctx context.Context, tok, teamID string) ([]linearIssue, error) {
	type rawIssue struct {
		ID            string     `json:"id"`
		Identifier    string     `json:"identifier"`
		Previous      []string   `json:"previousIdentifiers"`
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
		Cycle *struct {
			ID string `json:"id"`
		} `json:"cycle"`
		ProjectMilestone *struct {
			ID string `json:"id"`
		} `json:"projectMilestone"`
		LabelIDs []string `json:"labelIds"`
		Comments struct {
			Nodes []rawComment `json:"nodes"`
			Page  pageInfo     `json:"pageInfo"`
		} `json:"comments"`
		Attachments struct {
			Nodes []rawAttachment `json:"nodes"`
			Page  pageInfo        `json:"pageInfo"`
		} `json:"attachments"`
		Estimate         *float64 `json:"estimate"`
		InverseRelations struct {
			Nodes []rawRelation `json:"nodes"`
			Page  pageInfo      `json:"pageInfo"`
		} `json:"inverseRelations"`
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
				Previous:      r.Previous,
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
			if r.Cycle != nil {
				iss.CycleID = r.Cycle.ID
			}
			if r.ProjectMilestone != nil {
				iss.MilestoneID = r.ProjectMilestone.ID
			}
			if r.DueDate != nil {
				iss.DueDay, _ = importProvider.ParseDay(*r.DueDate)
			}
			iss.LabelIDs = r.LabelIDs
			iss.Estimate = r.Estimate
			for _, rel := range r.InverseRelations.Nodes {
				iss.addRelation(rel)
			}
			for _, c := range r.Comments.Nodes {
				iss.Comments = append(iss.Comments, c.comment())
			}
			for _, a := range r.Attachments.Nodes {
				iss.addAttachment(a)
			}
			if err := p.restOfIssue(ctx, tok, &iss, r.Comments.Page, r.Attachments.Page, r.InverseRelations.Page); err != nil {
				return nil, err
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

// connection is one page of a Linear list: its nodes, and where the next
// page starts.
type connection[T any] struct {
	Nodes []T      `json:"nodes"`
	Page  pageInfo `json:"pageInfo"`
}

// rootList reads every page of a list the workspace answers at the top of
// query, under root (users, teams, cycles): query takes each page's $cursor,
// and a runaway list stops at maxPages.
func rootList[T any](ctx context.Context, p *Provider, tok, query, root string) ([]T, error) {
	var out []T
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var resp map[string]connection[T]
		vars := map[string]any{}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, query, vars, &resp); err != nil {
			return nil, err
		}
		list := resp[root]
		out = append(out, list.Nodes...)
		if !list.Page.HasNextPage {
			break
		}
		cursor = list.Page.EndCursor
	}
	return out, nil
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

	if err := statusRules.Check(resp); err != nil {
		// Linear refuses a request over its rate limit with a 400 whose body
		// has the code RATELIMITED, not with a 429.
		var refused *importProvider.StatusError
		if errors.As(err, &refused) {
			if wait := rateLimited(resp.Header, []byte(refused.Body)); wait != nil {
				return wait
			}
		}
		return err
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("linear decode: %w", err)
	}
	if len(env.Errors) > 0 {
		if wait := limitOf(resp.Header, env.Errors); wait != nil {
			return wait
		}
		return fmt.Errorf("linear graphql: %s", env.Errors[0].Message)
	}
	if len(env.Data) == 0 {
		return errors.New("linear empty data envelope")
	}
	return json.Unmarshal(env.Data, out)
}

// ─── Auth ─────────────────────────────────────────────────────────

// loadToken loads + lazily refreshes the OAuth/PAT token for the
// import job's owner. PATs (no refresh_token) bypass the OAuth path
// inside FreshAccessToken and return as-is.
func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	return tokenFor(ctx, *j.TriggeredBy)
}

// tokenFor is loadToken's token for owner's stored connection.
func tokenFor(ctx context.Context, owner uuid.UUID) (string, error) {
	t, err := importModels.LoadToken(ctx, providerName, owner)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty linear token")
	}
	if t.RefreshToken != "" {
		fresh, ferr := importProvider.FreshAccessToken(ctx, linearOAuthConfig(), providerName, owner)
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
	desc := markdownToHTML(iss.Description) + relationLines(iss)

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
	fields := append(groupValues(iss.Groups), milestoneValue(iss.Milestone)...)

	return importProvider.SourceTask{
		SourceID:        iss.ID,
		SourceKey:       iss.Identifier, // ENG-123
		FormerKeys:      iss.Previous,
		ParentTaskID:    iss.ParentID,
		ProjectSourceID: projectID,
		CycleSourceID:   iss.CycleID,
		Name:            truncate(iss.Title, 256),
		Description:     desc,
		Status:          status,
		Priority:        priority,
		Labels:          append([]string{}, iss.Labels...),
		AssigneeIds:     assignees,
		CreatedBy:       iss.CreatorID,
		StartDate:       iss.StartedAt,
		DueDay:          iss.DueDay,
		Created:         iss.CreatedAt,
		Updated:         iss.UpdatedAt,
		Completed:       iss.CompletedAt != nil,
		EstimatePoints:  iss.Estimate,
		AttachmentRefs:  atts,
		CommentCount:    len(iss.Comments),
		Dependencies:    blockedBy(iss.BlockedBy),
		Fields:          fields,
		Metadata: map[string]any{
			"linear_url":        iss.URL,
			"linear_identifier": iss.Identifier, // e.g. "ENG-123"
			"linear_state_type": iss.StateType,
			"linear_priority":   iss.Priority,
		},
	}
}

// highestNumber is the highest number the issues keep from their keys
// (ENG-123's 123).
func highestNumber(issues []*linearIssue) int {
	n := 0
	for _, iss := range issues {
		_, k := importProvider.SplitKey(iss.Identifier)
		n = max(n, k)
	}
	return n
}

func blockedBy(ids []string) []importProvider.SourceDependency {
	var out []importProvider.SourceDependency
	for _, id := range ids {
		out = append(out, importProvider.SourceDependency{BlockerSourceID: id})
	}
	return out
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
	// Visibility is who Linear shows the team and its issues to: everyone
	// ("public"), or some people only (keptTeams).
	Visibility string
	// Its cycle settings, in weeks (cycles.go).
	CyclesEnabled bool
	CycleWeeks    int
	CooldownWeeks int
	Upcoming      int
	// Its estimate settings, and its parent's id for a sub-team (estimates.go).
	EstimationType      string
	EstimationExtended  bool
	EstimationAllowZero bool
	DefaultEstimate     float64
	InheritEstimation   bool
	ParentID            string
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
	Previous      []string // keys it had before it moved teams (previousIdentifiers)
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
	DueDay        importProvider.Day // dueDate: a day with no time, written where the team is
	StateID       string
	StateName     string
	StateType     string
	AssigneeID    string
	CreatorID     string
	ParentID      string
	TeamID        string
	ProjectID     string
	CycleID       string
	MilestoneID   string           // projectMilestone: the milestone of its project it's in
	Milestone     *linearMilestone // that milestone, as the workspace lists it (buildSnapshot)
	Labels        []string         // the names of its labels outside any group: tags
	LabelIDs      []string         // labelIds: every label, resolved by placeLabels
	Groups        []labelPick      // its label in each label group: a select field
	BlockedBy     []string         // ids of the issues that block this one
	Relations     []linearRelation // duplicate and related links that point at it
	DuplicateOf   []issueRef       // what it duplicates (linkRelations)
	Duplicates    []issueRef       // the issues that duplicate it
	Related       []issueRef       // the issues related to it
	Estimate      *float64         // points, as the team estimates
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
	if err := p.gql(ctx, access, discoverQuery, nil, &resp); err != nil {
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

// planStatuses is the plan's status values, sorted, each workflow state once
// by its name (an issue's status, issueToSourceTask), with its type as its
// class; a state with no name is listed by its type. Pure.
func planStatuses(issues []linearIssue) ([]string, map[string]string) {
	set := map[string]struct{}{}
	kinds := map[string]string{}
	for _, iss := range issues {
		name, typ := strings.ToLower(strings.TrimSpace(iss.StateName)), strings.ToLower(strings.TrimSpace(iss.StateType))
		switch {
		case name != "":
			set[name] = struct{}{}
			if typ != "" {
				kinds[name] = typ
			}
		case typ != "":
			set[typ] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, kinds
}

// source is the person as the import takes them: a guest is outside the
// company, and someone deactivated has left (never invited).
func (u linearUser) source() importProvider.SourceUser {
	return importProvider.SourceUser{
		SourceID:    u.ID,
		DisplayName: helpers.FirstNonEmpty(u.Name, u.DisplayName),
		Login:       u.DisplayName,
		Email:       u.Email,
		AvatarURL:   u.AvatarURL,
		IsExternal:  u.Guest,
		Left:        !u.Active,
		Metadata:    map[string]any{"linear_id": u.ID},
	}
}

// issueLabelsQuery reads the workspace's labels once, groups and archived
// ones included (an old issue keeps an archived label): an issue then reads
// only its labels' ids, so none is capped. 250 labels with a parent each cost
// about 600 points, well under the 10,000 cap.
const issueLabelsQuery = `
query ($cursor: String) {
  issueLabels(first: 250, after: $cursor, includeArchived: true) {
    nodes { id name color isGroup parent { id } }
    pageInfo { hasNextPage endCursor }
  }
}`

// linearLabel is one of the workspace's labels. A group (isGroup) holds
// labels of which an issue takes one, as a single-select field holds options.
type linearLabel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	IsGroup  bool   `json:"isGroup"`
	ParentID string `json:"-"`
	Parent   *struct {
		ID string `json:"id"`
	} `json:"parent"`
}

// labelPick is an issue's label in one label group, with the group's labels
// as the field's options, by name.
type labelPick struct {
	GroupID, GroupName, LabelID, LabelName string
	Options                                []importProvider.SourceOption
}

// fetchLabels is the workspace's labels by id, a page at a time.
func (p *Provider) fetchLabels(ctx context.Context, tok string) (map[string]linearLabel, error) {
	nodes, err := rootList[linearLabel](ctx, p, tok, issueLabelsQuery, "issueLabels")
	if err != nil {
		return nil, err
	}
	out := make(map[string]linearLabel, len(nodes))
	for _, l := range nodes {
		if l.Parent != nil {
			l.ParentID = l.Parent.ID
		}
		out[l.ID] = l
	}
	return out, nil
}

// placeLabels sorts an issue's labels: one in a label group is that group's
// pick (a single-select field), and the rest are tags by name. An id the
// workspace no longer lists (a label since deleted) is left out.
func placeLabels(iss *linearIssue, labels map[string]linearLabel) {
	iss.Labels, iss.Groups = nil, nil
	for _, id := range iss.LabelIDs {
		l, ok := labels[id]
		if !ok || l.IsGroup || l.Name == "" {
			continue
		}
		g, grouped := labels[l.ParentID]
		if !grouped || !g.IsGroup {
			iss.Labels = append(iss.Labels, l.Name)
			continue
		}
		iss.Groups = append(iss.Groups, labelPick{
			GroupID: g.ID, GroupName: g.Name, LabelID: l.ID, LabelName: l.Name,
			Options: groupOptions(g.ID, labels),
		})
	}
}

// groupOptions is a label group's labels as a select field's options, by
// name, each in its label's colour.
func groupOptions(group string, labels map[string]linearLabel) []importProvider.SourceOption {
	var out []importProvider.SourceOption
	for _, l := range labels {
		if l.ParentID == group && !l.IsGroup && l.Name != "" {
			out = append(out, importProvider.SourceOption{SourceID: l.ID, Label: l.Name, Color: importProvider.PaletteColor(l.Color)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// groupValues are an issue's label-group picks as select field values; the
// field is the group, the same one on every issue.
func groupValues(picks []labelPick) []importProvider.SourceFieldValue {
	var out []importProvider.SourceFieldValue
	for _, g := range picks {
		out = append(out, importProvider.SourceFieldValue{
			Field: importProvider.SourceField{SourceID: "label-group:" + g.GroupID, Name: g.GroupName, Type: importProvider.FieldSelect, Options: g.Options},
			Value: g.LabelID,
			Text:  g.LabelName,
		})
	}
	return out
}
