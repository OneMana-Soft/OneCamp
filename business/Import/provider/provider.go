// Package provider defines the contract every import source implements.
//
// The orchestrator (business/Import/orchestrator.go) is the one consumer
// of this interface. It calls Plan to materialise chunks, then runs
// per-stage worker pools that pull chunks and dispatch into Iter*
// streams plus FetchAttachment.
//
// Each provider is registered at init() time via Register(). The
// controller resolves the active provider for a job by Job.Provider.
package provider

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// ─── Source DTOs ──────────────────────────────────────────────────────

// SourceUser is the provider-neutral user shape. Each provider maps its
// raw user format into this; the generic user resolver handles the rest.
type SourceUser struct {
	SourceID    string
	Email       string
	DisplayName string
	Login       string // workspace handle / username
	AvatarURL   string
	IsBot       bool
	IsExternal  bool
	Metadata    map[string]any
}

// SourceTeam is the provider's "team" / "workspace tier" shape. Trello
// has organisations, Asana has teams, Jira has projects-grouped-by-cat.
// Providers that don't have teams emit a single synthetic team named
// after the workspace.
type SourceTeam struct {
	SourceID    string
	Name        string
	Description string
	MemberIds   []string // source ids; resolved via id_map at write time
	AdminIds    []string
	CreatedBy   string
	Created     time.Time
	Metadata    map[string]any
}

// SourceProject covers Asana projects, Jira projects, Trello boards,
// Notion task databases, Todoist projects.
type SourceProject struct {
	SourceID     string
	Name         string
	Description  string
	TeamSourceID string // empty → falls under the synthetic default team
	MemberIds    []string
	AdminIds     []string
	CreatedBy    string
	Created      time.Time
	Archived     bool
	// SourceKey is the source's own project key (Jira's ENG): the project
	// keeps it when no other project has it, so ENG-123 still finds its task.
	SourceKey string
	Fields    []SourceField  // custom fields, in the source's order (fields.go)
	Metadata  map[string]any // includes per-provider URL, color, etc.
}

// SourceTask covers tasks/issues/cards/database-rows.
type SourceTask struct {
	SourceID        string
	SourceKey       string // the source's own key (ENG-123): the task keeps its number
	ParentTaskID    string // empty for top-level
	ProjectSourceID string
	Name            string
	Description     string // HTML; provider must render its native rich text
	Status          string // raw — clamped via per-import status map
	Priority        string // raw — clamped via priority map
	Labels          []string
	AssigneeIds     []string // first becomes task_assignee, rest → @mentions
	CreatedBy       string
	StartDate       *time.Time
	DueDate         *time.Time
	Created         time.Time
	Updated         time.Time
	Completed       bool
	EstimateMinutes int                // how long the source says it takes; 0 is none
	AttachmentRefs  []SourceAttachment // queued lazily by worker
	CommentCount    int                // hint; CommentsUnknown when the source can't say
	SubtaskCount    int                // hint
	Fields          []SourceFieldValue // custom field values (fields.go)
	Dependencies    []SourceDependency // the tasks this one waits on
	Metadata        map[string]any     // source URL, etc.
}

// SourceDependency is a task this one waits on: by its source id, how it
// waits (fs, ss, ff or sf as business/Task takes them; empty is finish to
// start) and the lag in days. It is linked once every task has landed.
type SourceDependency struct {
	BlockerSourceID string `json:"blocker"`
	Kind            string `json:"kind,omitempty"`
	LagDays         int    `json:"lag,omitempty"`
}

// CommentsUnknown is a SourceTask's CommentCount when the source can't count
// a task's comments without asking for them (Asana): the comments stage asks.
// A count of 0 is a task with none, and asks nothing.
const CommentsUnknown = -1

// TaskFileLister is a provider whose task files aren't in the task it reads
// (Asana): the comments stage lists them with the task's comments, and each
// is fetched as a file of the task.
type TaskFileLister interface {
	ListTaskFiles(ctx context.Context, j *importModels.Job, opts JobOptions, taskSourceId string) ([]SourceAttachment, error)
}

// SourceComment covers task/issue/card comments.
type SourceComment struct {
	SourceID       string
	TaskSourceID   string
	Body           string // HTML
	AuthorSourceID string
	Created        time.Time
	AttachmentRefs []SourceAttachment
	IsSystem       bool // Asana stories, Jira changelog entries, etc.
}

// SourceAttachment describes one file referenced from a parent task or
// comment. URL may be short-lived; the worker downloads eagerly.
type SourceAttachment struct {
	SourceID string
	Name     string
	URL      string
	Mime     string
	Size     int64
	Parent   SourceRef
	// Headers requested when fetching (e.g. provider's auth header).
	// Populated by the provider so the orchestrator can call
	// FetchAttachment provider-agnostically — or the provider may
	// override FetchAttachment entirely.
	Headers map[string]string
}

// SourceRef discriminates the parent of an attachment.
type SourceRef struct {
	Kind     string // "task" | "comment" | "project"
	SourceID string
}

// SourcePage is one page a provider brings, which becomes a doc.
type SourcePage struct {
	SourceID string
	// ParentSourceID is the page it sits under, among the pages of its tree;
	// "" for the tree's root.
	ParentSourceID string
	Title          string
	// Icon is the page's emoji, when it has one.
	Icon string
	// HTML is its content in the editor's own HTML, which an import reads as
	// foreign HTML. A link to another page of its tree is that page's SourceID
	// as a relative address, and becomes a link to its doc.
	HTML string
	// Files are what the HTML names by a relative address (a picture the
	// provider read, because its own address expires), by that address.
	Files map[string][]byte
	// Comments are the comments on the page itself, oldest first, as the
	// doc's comments; their TaskSourceID is unused.
	Comments []SourceComment
}

// PageSource is a provider that brings pages (CapPages). Its Plan queues one
// ChunkDocPages chunk per tree of pages, the tree's root as the chunk's
// ParentSourceId.
type PageSource interface {
	// PagesOf reads the tree of pages under root, every page after the page
	// it sits under; read says how many pages it has read so far, as it goes.
	PagesOf(ctx context.Context, j *importModels.Job, opts JobOptions, root string, read func(pages int)) ([]SourcePage, error)
}

// ─── Plan DTO ────────────────────────────────────────────────────────

// Plan is what BuildPlan returns. The orchestrator persists it in
// import_jobs.plan and the FE renders the confirmation screen from it.
type Plan struct {
	UserCount    int      `json:"user_count"`
	UserNew      int      `json:"user_new"`
	UserMerge    int      `json:"user_merge"`
	TeamCount    int      `json:"team_count"`
	ProjectCount int      `json:"project_count"`
	TaskCount    int      `json:"task_count"`
	PageCount    int      `json:"page_count,omitempty"` // pages that become docs (CapPages)
	SubtaskCount int      `json:"subtask_count"`
	CommentCount int      `json:"comment_count"`
	FileCount    int      `json:"file_count"`
	FileBytes    int64    `json:"file_bytes"`
	Warnings     []string `json:"warnings,omitempty"`
	// StatusValues is the unique set of source statuses found in the
	// data. The FE shows a dropdown per value letting the operator
	// pick the OneCamp target. Default proposed mapping (provider's
	// DefaultStatusMap()) is pre-filled.
	StatusValues   []string `json:"status_values,omitempty"`
	PriorityValues []string `json:"priority_values,omitempty"`
	// Slack-shape fields kept for backward-compat with the existing FE.
	ChannelCount    int `json:"channel_count,omitempty"`
	ChannelConflict int `json:"channel_conflict,omitempty"`
	MessageCount    int `json:"message_count,omitempty"`
	ThreadCount     int `json:"thread_count,omitempty"`
}

// ─── Provider interface ─────────────────────────────────────────────

// Capability flags let the orchestrator skip stages a provider doesn't
// produce. E.g., Slack has no Tasks; Trello has no Threads.
type Capability uint32

const (
	CapTeams Capability = 1 << iota
	CapProjects
	CapTasks
	CapSubtasks
	CapTaskComments
	CapAttachments
	// Slack-shaped capabilities (kept so the Slack provider fits the
	// same interface).
	CapChannels
	CapDMs
	CapMessages
	CapReactions
	// CapPages is a provider that brings pages, which become docs (PageSource).
	CapPages
)

// JobOptions are per-import operator knobs. The orchestrator and
// provider both read this from import_jobs.options. Each provider
// declares its own option keys via DescribeOptions.
type JobOptions = map[string]any

// Provider is the contract every source system implements.
//
// Lifecycle:
//  1. Validate(job, ctx)          — cheap structural checks.
//  2. Plan(job, ctx)              — count and queue.
//  3. Per stage: orchestrator pulls chunks and calls one of
//     IterUsers, IterTeams, IterProjects, IterTasksOfProject,
//     IterCommentsOfTask. The chunk's ParentSourceId tells the
//     provider which project/task to iterate.
//  4. FetchAttachment(att, w)     — stream bytes to MinIO.
//
// Each Iter* may emit a SourceXxx through its channel and any errors
// through the error channel; closing the source channel signals "done".
// The orchestrator uses a small buffered chan to apply backpressure.
type Provider interface {
	Name() string
	Capabilities() Capability
	SupportedSources() []string

	// Validate runs cheap structural / authentication checks.
	Validate(ctx context.Context, j *importModels.Job, opts JobOptions) error

	// Plan walks the source enough to produce counts, conflict warnings,
	// and the initial chunk queue. Returned chunks are persisted by the
	// orchestrator. Plan MUST be idempotent.
	Plan(ctx context.Context, j *importModels.Job, opts JobOptions) (*Plan, []*importModels.Chunk, error)

	// IterUsers streams every user the import should resolve.
	IterUsers(ctx context.Context, j *importModels.Job, opts JobOptions) (<-chan SourceUser, <-chan error)

	// IterTeams streams team-tier entities. Empty stream is fine for
	// providers that don't have teams.
	IterTeams(ctx context.Context, j *importModels.Job, opts JobOptions) (<-chan SourceTeam, <-chan error)

	// IterProjects streams projects.
	IterProjects(ctx context.Context, j *importModels.Job, opts JobOptions) (<-chan SourceProject, <-chan error)

	// IterTasksOfProject streams tasks for a specific project. The
	// orchestrator schedules one chunk per project, so this is the per-
	// chunk iteration entry point.
	IterTasksOfProject(ctx context.Context, j *importModels.Job, opts JobOptions, projectSourceId string) (<-chan SourceTask, <-chan error)

	// IterSubtasksOfTask streams subtasks for a specific task. May be
	// empty if the provider's subtasks come back inside the parent
	// task's stream (Trello checklists work this way).
	IterSubtasksOfTask(ctx context.Context, j *importModels.Job, opts JobOptions, taskSourceId string) (<-chan SourceTask, <-chan error)

	// IterCommentsOfTask streams comments for a specific task.
	IterCommentsOfTask(ctx context.Context, j *importModels.Job, opts JobOptions, taskSourceId string) (<-chan SourceComment, <-chan error)

	// FetchAttachment downloads one attachment to dest. Returns
	// (mime, size, err). If the URL has expired, return ErrAttachmentGone
	// so the worker can mark a placeholder without retrying.
	FetchAttachment(ctx context.Context, j *importModels.Job, opts JobOptions, att SourceAttachment, dest io.Writer) (mime string, size int64, err error)

	// DefaultStatusMap returns the proposed source-status → OneCamp
	// status map used as the initial state of the operator's confirmation
	// screen. Keys MUST be lower-cased.
	DefaultStatusMap() map[string]string

	// DefaultPriorityMap is the priority equivalent.
	DefaultPriorityMap() map[string]string
}

// ErrAttachmentGone signals the URL is no longer valid (404/410). The
// worker creates a placeholder attachment and continues.
var ErrAttachmentGone = errors.New("attachment gone (404/410)")

// ErrTokenRejected is a provider refusing the credentials themselves: a token
// that is wrong, revoked or expired. Each provider says so in its own words
// (TokenRejected), and the screens test for this, not for the words, to say
// "it didn't accept that token" and offer to reconnect instead of relaying an
// HTTP status.
var ErrTokenRejected = errors.New("the provider didn't accept the token")

// TokenRejected is a provider's own message for a refused token.
// errors.Is(err, ErrTokenRejected) holds for it.
type TokenRejected struct{ Msg string }

func (e *TokenRejected) Error() string { return e.Msg }

// Is makes a TokenRejected match ErrTokenRejected.
func (e *TokenRejected) Is(target error) bool { return target == ErrTokenRejected }

// ErrRateLimited signals the provider hit a 429 / quota limit. The
// worker uses ResetChunkForRetry so the chunk's attempt count is not
// burned. RetryAfter (when non-zero) is honoured before the next claim.
type ErrRateLimited struct {
	RetryAfter time.Duration
	Reason     string
}

func (e *ErrRateLimited) Error() string {
	if e.Reason != "" {
		return "rate limited: " + e.Reason
	}
	return "rate limited"
}

// IsRateLimited matches *ErrRateLimited and returns its RetryAfter.
func IsRateLimited(err error) (time.Duration, bool) {
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

// ─── Registry ─────────────────────────────────────────────────────────

// registry maps provider name → Provider. Populated by each provider's
// init() via Register(). Lookup is concurrency-safe because the map is
// frozen after init().
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Provider, 8)
)

// Register installs a provider under its Name(). Subsequent calls with
// the same name overwrite — useful for testing.
func Register(p Provider) {
	if p == nil {
		return
	}
	registryMu.Lock()
	registry[p.Name()] = p
	registryMu.Unlock()
}

// Get returns the registered provider, or nil if not found.
func Get(name string) Provider {
	registryMu.RLock()
	p := registry[name]
	registryMu.RUnlock()
	return p
}

// Names returns every registered provider name. Used by the discovery
// endpoint and by route registration.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	return out
}

// JobCleaner is implemented by providers that want a callback when a
// job reaches a terminal state. Used to evict per-job caches (e.g.,
// Trello board snapshot, Asana workspace gid, Notion database list).
//
// Optional: providers without per-job state simply don't implement it.
type JobCleaner interface {
	CleanupJob(jobId string)
}
