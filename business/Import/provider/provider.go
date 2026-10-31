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

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
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
	// Left is someone who left the source (deactivated or deleted): they come
	// as a placeholder, so their history keeps their name, and are never
	// offered an invitation (people.go).
	Left     bool
	Metadata map[string]any
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
	// HighestNumber is the highest number the project's tasks keep from their
	// source keys (ENG-123's 123), sub-tasks' included, or 0. The project
	// numbers a task made with no number of its own after it (Linear's
	// milestones), so it takes none a task still to come keeps.
	HighestNumber int
	Fields        []SourceField  // custom fields, in the source's order (fields.go)
	Metadata      map[string]any // includes per-provider URL, color, etc.
	// Cycles are the source's cycles that hold the project's tasks, and
	// CycleSchedule is how the source makes them (cycles.go).
	Cycles        []SourceCycle
	CycleSchedule *SourceCycleSchedule
	// EstimateScale is the scale its team estimates on in the source; nil,
	// hours (or no estimates).
	EstimateScale *SourceEstimateScale
}

// SourceEstimateScale is a source's estimate scale, which the project keeps
// here (business/EstimateScale): Kind is exponential, fibonacci, linear or
// tshirt.
type SourceEstimateScale struct {
	Kind             string
	Extended         bool // the scale's two larger sizes
	AllowZero        bool // zero is a size
	CountUnestimated bool // a task with no points counts as one
}

// SourceCycle is a time-boxed stretch of the source's work that held some of
// a project's tasks (Linear's cycle).
type SourceCycle struct {
	SourceID    string
	Name        string // as the source shows it, so Linear's unnamed 12th is "Cycle 12"
	StartsAt    time.Time
	EndsAt      time.Time
	CompletedAt *time.Time // when the source closed it
	Done        int        // the project's tasks it finished, for a closed one
}

// SourceCycleSchedule is how the source makes a team's cycles, which the
// project keeps doing here (business/Cycle's schedule).
type SourceCycleSchedule struct {
	Weeks         int
	CooldownWeeks int
	Upcoming      int
	RollOver      bool      // a cycle's unfinished tasks move on to the next
	Anchor        time.Time // a cycle's start: the others fall on its weeks
}

// TaskType is a built-in kind of work a source's task comes as
// (business/WorkTaxonomy). A task with none is a Task.
type TaskType string

// TaskMilestone marks a point in a project, on the task's due day (Linear's
// project milestones).
const TaskMilestone TaskType = "milestone"

// SourceTask covers tasks/issues/cards/database-rows.
type SourceTask struct {
	SourceID        string
	SourceKey       string   // the source's own key (ENG-123): the task keeps its number
	FormerKeys      []string // keys the task had before in the source (Linear's previousIdentifiers)
	ParentTaskID    string   // empty for top-level
	ProjectSourceID string
	Type            TaskType // the built-in kind it comes as; "" is a Task
	Name            string
	Description     string // HTML; provider must render its native rich text
	Status          string // raw — clamped via per-import status map
	Priority        string // raw — clamped via priority map
	Labels          []string
	AssigneeIds     []string // first becomes task_assignee, rest → @mentions
	CreatedBy       string
	StartDate       *time.Time // a time the source gave; a date with no time is StartDay
	DueDate         *time.Time // a time the source gave; a date with no time is DueDay
	StartDay        Day        // a date the source keeps with no time (day.go); never set with StartDate
	DueDay          Day        // a date the source keeps with no time (day.go); never set with DueDate
	Created         time.Time
	Updated         time.Time
	Completed       bool
	EstimateMinutes int                // how long the source says it takes; 0 is none
	EstimatePoints  *float64           // how big it is on its project's scale; nil is none
	AttachmentRefs  []SourceAttachment // queued lazily by worker
	CommentCount    int                // hint; CommentsUnknown when the source can't say
	SubtaskCount    int                // hint
	Fields          []SourceFieldValue // custom field values (fields.go)
	Dependencies    []SourceDependency // the tasks this one waits on
	CycleSourceID   string             // the cycle it is in: one of its project's Cycles
	Metadata        map[string]any     // source URL, etc.
	// Restricted is why the source keeps the task to some of its people, as
	// the import's report says it ("" is a task everyone in its project may
	// see). It comes in restricted (WA2), never readable more widely here
	// than there: business/Import restricted.go.
	Restricted string
	// Audience are the people (source ids) the source lets see a restricted
	// task besides its assignee, such as Jira's reporter. Its creator isn't
	// one for having made it.
	Audience []string
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
	// Restricted is why the source keeps the comment from some of the people
	// who see its task, as the import's report says it ("" is a comment
	// everyone who sees its task may see). It stays in the source: the report
	// counts it on its task, without its text (restricted.go).
	Restricted string
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

// SourceBoard is one board a provider brings, drawn: its elements as the
// board's canvas keeps them (business/Board/scene), the pictures they show,
// and what the import's report says about it.
type SourceBoard struct {
	SourceID    string
	Title       string
	Description string
	// Elements are Excalidraw elements, in the order they're drawn. An image
	// names its picture by fileId.
	Elements []map[string]any
	// Pictures are the board's images, each read through FetchAttachment.
	// One that can't be read or kept becomes a labelled placeholder.
	Pictures []BoardPicture
	// Notes say what came over approximated and what couldn't come.
	Notes []BoardNote
	// Items is how many of the source's items were read.
	Items int
}

// BoardPicture is one picture on a board: the fileId its image elements name,
// and where the provider reads it.
type BoardPicture struct {
	FileID     string
	Attachment SourceAttachment
}

// BoardNote is one line of a board's report: a code, what it says, and how
// many items it is about.
type BoardNote struct {
	Code  string
	Msg   string
	Count int
}

// BoardSource is a provider that brings boards (CapBoards). Its Plan queues
// one ChunkBoards chunk per board, the board's id as the chunk's
// ParentSourceId.
type BoardSource interface {
	// BoardOf reads one board and draws it; read says how many of its items
	// have been read so far, as it goes.
	BoardOf(ctx context.Context, j *importModels.Job, opts JobOptions, boardID string, read func(items int)) (*SourceBoard, error)
}

// TableSource is a provider that brings bases of tables (CapTables): their
// tables, fields of OneCamp's kinds, rows and views, in OneCamp's terms
// (dataTableBusiness.ImportBase), which the tables worker makes through the
// one importer of every source's tables. Its Plan queues one ChunkTableBase
// chunk per base, the base as the chunk's ParentSourceId.
type TableSource interface {
	// BaseOf reads the base: its tables and fields at once, its rows as the
	// importer asks for them (a page at a time); read says how many rows it
	// has read so far, as it goes.
	BaseOf(ctx context.Context, j *importModels.Job, opts JobOptions, base string, read func(rows int)) (dataTableBusiness.ImportBase, error)
}

// PersonRef is a person in a people cell a TableSource brings, by email:
// the tables stage matches it to a member of the workspace before the row is
// written, and leaves out one who isn't a member. It is the tables'
// PersonRef, which a sheet's people chips are guessed into too.
type PersonRef = dataTableBusiness.PersonRef

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
	PageCount    int      `json:"page_count,omitempty"`  // pages that become docs (CapPages)
	BoardCount   int      `json:"board_count,omitempty"` // boards that come over (CapBoards)
	ItemCount    int      `json:"item_count,omitempty"`  // what is on them
	TableCount   int      `json:"table_count,omitempty"` // tables of the bases brought (CapTables)
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
	// StatusKinds is each status's own class in the source, by its lower-cased
	// name (Linear's state type, Jira's status category), so a status with a
	// name of the team's own is proposed by its class, not as To do.
	StatusKinds map[string]string `json:"status_kinds,omitempty"`
	// StatusProposals is the OneCamp status the plan proposes for each value
	// (StatusDefaults), for the operator to confirm.
	StatusProposals map[string]string `json:"status_proposals,omitempty"`
	// Columns are an uploaded table's columns (a CSV's), each with what it is
	// read as: a task's own property, or a field of the kind its values
	// suggest, which the admin can change before the import runs.
	Columns []PlanColumn `json:"columns,omitempty"`
	// People are the people a file names, each with how they will be matched
	// to someone here (BuildPlan says), so the admin sees who becomes whom
	// before anything is written.
	People []PlanPerson `json:"people,omitempty"`
	// Slack-shape fields kept for backward-compat with the existing FE.
	ChannelCount    int `json:"channel_count,omitempty"`
	ChannelConflict int `json:"channel_conflict,omitempty"`
	MessageCount    int `json:"message_count,omitempty"`
	ThreadCount     int `json:"thread_count,omitempty"`
}

// PlanColumn is one column of an uploaded table.
type PlanColumn struct {
	Name string `json:"name"` // as the file's header has it
	// Property is the task's own property it fills (title, status, due…), or
	// "" for a column that becomes a field.
	Property string `json:"property,omitempty"`
	// Kind is the field it becomes (a Field* type, or KindSkip), and Guess the
	// kind its values suggest. A field column only.
	Kind     string `json:"kind,omitempty"`
	Guess    string `json:"guess,omitempty"`
	Currency string `json:"currency,omitempty"` // a money field's code
	Values   int    `json:"values"`             // its cells with something in them
}

// KindSkip is a column the admin chose not to bring.
const KindSkip = "skip"

// PlanPerson is someone a file names (a task's assignee or maker, a person
// field's value).
type PlanPerson struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Rows     int    `json:"rows"` // the rows that name them
	// MatchedBy is how they were matched: "email", "name", or an earlier
	// import's "workspace_map"; "" is no one, and they come as a placeholder.
	MatchedBy string `json:"matched_by,omitempty"`
	Member    string `json:"member,omitempty"`    // whom they matched, by name
	SameName  int    `json:"same_name,omitempty"` // members who share their name, when that is why no one matched
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
	// CapBoards is a provider that brings boards (BoardSource).
	CapBoards
	// CapTables is a provider that brings bases of tables (TableSource).
	CapTables
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
