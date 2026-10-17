package business

// Anchored Threads: a conversation pinned to a place in a resource (docs
// programme, PROGRAM.md §4 #18; A8). The thread lives in Postgres (the threads
// migration, migrations/*_threads.up.sql) and its comments stay where the
// resource keeps them: a doc's are its Comment nodes in the graph, each tagged
// with comment_thread_id. An anchor never re-attaches silently (D-9): when its
// text is gone, or the doc was rebuilt or restored into a new lineage, the web
// app shows the thread as detached with its quote, and only an exact match is
// offered back as "Reattach here", which someone must choose (Reanchor).
//
// The core knows nothing of docs. Each kind of resource plugs in through a
// Resource (resources.go): who may do what there (through the Access
// Resolver), where its comments live, and which anchor kinds it takes. Docs
// are the first kind (docResource.go); chat messages, event and call
// comments, table cells and tasks are meant to follow behind the same seam.
//
// TODAY'S FLAT DOC COMMENTS (decided 12 Oct 2026, A8): kept, and read through
// a compatibility path, not migrated. A doc comment with no thread reads as a
// page-level ('none') thread of its own, whose id is the comment's uuid; the
// first write to it (a reply, resolve, reopen or reattach) makes it a real
// thread under that same id (service.go: load, adoptIfLegacy and assemble).
// Why not a one-time migration: the old comment route and panel stay live
// until B7 replaces them, so flat comments keep arriving after any migration
// would have run; reading them as threads covers every one whenever it was
// written, writes nothing at boot or on a read, and survives the demo's
// nightly restore and `demoseed --refresh` without a re-run.
//
// The web app's twin is lib/anchors/types.ts and components/threads/types.ts;
// the JSON below is what passes between them.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// AnchorKind is what a thread is pinned to.
type AnchorKind string

const (
	AnchorYRange  AnchorKind = "yrange"  // a range of a live doc, as Yjs relative positions
	AnchorBlock   AnchorKind = "block"   // a whole block, by its block id
	AnchorMessage AnchorKind = "message" // a chat message
	AnchorTime    AnchorKind = "time"    // a moment in a recording or call
	AnchorCell    AnchorKind = "cell"    // a table cell
	AnchorNone    AnchorKind = "none"    // the resource as a whole
)

// Anchor is where a thread is pinned.
type Anchor struct {
	Kind AnchorKind `json:"kind"`
	// Start and End are Yjs relative positions, encoded, for AnchorYRange.
	Start []byte `json:"start,omitempty"`
	End   []byte `json:"end,omitempty"`
	// BlockID is the block for AnchorBlock, and the block the range began in for
	// AnchorYRange.
	BlockID string `json:"block_id,omitempty"`
	// Lineage is the doc lineage the positions belong to. A different lineage
	// detaches the thread.
	Lineage string `json:"lineage,omitempty"`
	// Quote is the text the thread was started on, kept when it is detached.
	Quote string `json:"quote,omitempty"`
	// Meta is the kind's own detail: a message id, a time in milliseconds, a
	// row and field.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// Status is a thread's state.
type Status string

const (
	StatusOpen     Status = "open"
	StatusResolved Status = "resolved"
)

// Thread is one anchored conversation.
type Thread struct {
	ID           string     `json:"id"`
	ResourceKind string     `json:"resource_kind"`
	ResourceUUID string     `json:"resource_uuid"`
	Anchor       Anchor     `json:"anchor"`
	Status       Status     `json:"status"`
	ResolvedBy   string     `json:"resolved_by,omitempty"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
	TaskUUID     string     `json:"task_uuid,omitempty"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Person is someone as a comment shows them.
type Person struct {
	UUID       string `json:"user_uuid"`
	Name       string `json:"user_name,omitempty"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
}

// Comment is one comment in a thread. Its JSON keys are the ones every comment
// in the product has (comment_uuid, comment_text, comment_by…), so the web app
// renders it as it renders any comment. Attachments and reactions are the
// resource's own shapes, passed on as they are.
type Comment struct {
	ID          string          `json:"comment_uuid"`
	ThreadID    string          `json:"thread_id"`
	Text        string          `json:"comment_text"`
	By          Person          `json:"comment_by"`
	CreatedAt   time.Time       `json:"comment_created_at"`
	UpdatedAt   *time.Time      `json:"comment_updated_at,omitempty"`
	Attachments json.RawMessage `json:"comment_attachments,omitempty"`
	Reactions   json.RawMessage `json:"comment_reactions,omitempty"`
}

// Role is how someone takes part in a thread. A person keeps the highest role
// they have had: author > replier > mentioned > reader.
type Role string

const (
	RoleAuthor    Role = "author"
	RoleReplier   Role = "replier"
	RoleMentioned Role = "mentioned"
	// RoleReader only opened the thread (MarkRead). Readers are not told of
	// its replies: they are not taking part.
	RoleReader Role = "reader"
)

// Participant is one person in a thread, and when they last read it.
type Participant struct {
	UserUUID   string     `json:"user_uuid"`
	Role       Role       `json:"role"`
	LastReadAt *time.Time `json:"last_read_at,omitempty"`
}

// View is a thread as one person sees it: its comments, oldest first, who
// takes part, whether it has news for them, and what they may do with it.
type View struct {
	Thread
	Comments     []Comment     `json:"comments"`
	Participants []Participant `json:"participants"`
	// Unread is whether someone else commented after the person last read it.
	Unread bool `json:"unread"`
	// Legacy is a comment from before threads, read as a page-level thread of
	// its own (the compatibility path); its id is the comment's.
	Legacy bool `json:"legacy,omitempty"`
	// CanReply is whether the person may reply; CanResolve whether they may
	// also resolve, reopen and reattach it.
	CanReply   bool `json:"can_reply"`
	CanResolve bool `json:"can_resolve"`
	// CanModerate is whether they may remove anyone's comment in it
	// (moderation.go): the resource's owner, full access, or an admin.
	CanModerate bool `json:"can_moderate"`
}

// Body is a comment as someone writes it: the editor's HTML, and attachments
// in the resource's own shape (for a doc, the doc comment's attachments).
type Body struct {
	HTML        string          `json:"body"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
}

// Filter narrows a list. A zero Filter is every thread.
type Filter struct {
	// Status is StatusOpen, StatusResolved, or "" for both.
	Status Status
}

// Service is the threads service. Every call goes through the resource's
// adapter and the Access Resolver: reading needs view access; creating and
// replying need comment access; resolving, reopening and reattaching need
// comment access and being the thread's author, or edit or full access. A
// deleted resource takes none of them.
type Service interface {
	// Create starts a thread at an anchor, with its first comment.
	Create(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, anchor Anchor, body Body) (View, error)
	// Reply adds a comment to a thread. A resolved thread stays resolved.
	Reply(ctx context.Context, who accessBusiness.Subject, threadID string, body Body) (Comment, error)
	Resolve(ctx context.Context, who accessBusiness.Subject, threadID string) (Thread, error)
	Reopen(ctx context.Context, who accessBusiness.Subject, threadID string) (Thread, error)
	// List is a resource's threads, oldest first, with their comments. Threads
	// whose comments are all gone are left out.
	List(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, f Filter) ([]View, error)
	// Get is one thread, as List shows it.
	Get(ctx context.Context, who accessBusiness.Subject, threadID string) (View, error)
	// Reanchor moves a thread to a new anchor. Only ever on someone's explicit
	// choice ("Reattach here"), never on its own.
	Reanchor(ctx context.Context, who accessBusiness.Subject, threadID string, anchor Anchor) (Thread, error)
	// MarkRead records that who has read the thread up to now.
	MarkRead(ctx context.Context, who accessBusiness.Subject, threadID string) error
}

var (
	// ErrUnavailable is a kind of resource threads can't reach: no adapter.
	ErrUnavailable = errors.New("threads: not available for this kind of resource")
	// ErrNotFound is no such thread, or no such resource.
	ErrNotFound = errors.New("threads: not found")
	// ErrForbidden is something the person may not do; a *Refusal says what.
	ErrForbidden = errors.New("threads: not allowed")
	// ErrGone is a resource that is deleted (in the trash).
	ErrGone = errors.New("threads: the resource is deleted")
)

// Invalid is a request that can't be done as asked, with what to say to the
// person.
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

// Refusal is a permission refusal, with what to say to the person. It is an
// ErrForbidden.
type Refusal struct{ Msg string }

func (e *Refusal) Error() string { return e.Msg }

// Is makes a Refusal an ErrForbidden for errors.Is.
func (e *Refusal) Is(target error) bool { return target == ErrForbidden }

// Threads is the service everything asks.
var Threads Service = newService(postgresStore{}, Resources)
