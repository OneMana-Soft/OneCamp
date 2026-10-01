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
	Metadata     map[string]any // includes per-provider URL, color, etc.
}

// SourceTask covers tasks/issues/cards/database-rows.
type SourceTask struct {
	SourceID        string
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
	AttachmentRefs  []SourceAttachment // queued lazily by worker
	CommentCount    int                // hint
	SubtaskCount    int                // hint
	Metadata        map[string]any     // source URL, custom fields, etc.
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
