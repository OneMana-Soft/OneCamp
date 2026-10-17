package business

// Edit Proposals: a proposed change to a resource, reviewed before it applies
// (docs programme, PROGRAM.md §4 #19 and D-10; A9 builds it). One model serves
// people's suggestions, inline AI and agents. A proposal is anchored like a thread,
// carries the content it was made against (its base) and that content's hash, and
// splits into hunks that are accepted one by one. Accepting applies the chosen
// hunks as one Yjs transaction: in the reviewer's editor, or on the server through
// the Live Doc Writer (services/collab ApplyProposal). A hunk whose text changed
// since can only be inserted below. A locked doc takes no new proposals.
//
// The web app's twin is lib/proposals/types.ts; the JSON below is what passes
// between them. The diff and the apply are lib/proposals/core.js on the web and
// live/ops/proposalCore.js in the collaboration service, one file in two places.
//
// The service is generic over the kind of resource: a kind registers how access
// is decided and how a step runs (Kinds.Register). Docs register here (docKind.go);
// a calendar's proposed schedule change or a recap's AI edit registers its own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
)

// AuthorKind is who proposed a change.
type AuthorKind string

const (
	AuthorPerson AuthorKind = "person"
	AuthorAgent  AuthorKind = "agent"
	AuthorAI     AuthorKind = "ai" // inline AI, asked by the person who reviews it
)

// Status is where a proposal stands.
type Status string

const (
	StatusOpen     Status = "open"
	StatusAccepted Status = "accepted"
	StatusPartial  Status = "partial" // every hunk decided, some accepted
	StatusDeclined Status = "declined"
	StatusStale    Status = "stale" // the target changed; it can only be inserted below
	// StatusWithdrawn is a proposal its author took back.
	StatusWithdrawn Status = "withdrawn"
	// statusPreparing is a server-made proposal whose hunks are being worked out
	// against the live doc. Never listed, never returned.
	statusPreparing Status = "preparing"
)

// Hunk decisions.
const (
	DecisionAccepted = "accepted"
	DecisionDeclined = "declined"
)

// Hunk is one reviewable piece of a proposal.
type Hunk struct {
	ID string `json:"id"`
	// Op is "insert", "delete" or "replace".
	Op string `json:"op"`
	// Level is "block" (whole blocks change) or "inline" (words inside one block).
	Level string `json:"level"`
	// Path, From and To place it in the base: Path is child indices down from the
	// target; From and To are child indices there (block) or offsets into that
	// block's text (inline).
	Path []int `json:"path"`
	From int   `json:"from"`
	To   int   `json:"to"`
	// Before and After are the hunk's content as codec JSON (lib/codec): arrays of
	// nodes, blocks or inline. A reviewer's diff and a server apply read the same.
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
	// Start and End are Yjs relative positions of its two ends, base64, so it is
	// found again wherever edits around it moved it.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// Decision is "", "accepted" or "declined".
	Decision  string     `json:"decision,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// Proposal is one proposed change.
type Proposal struct {
	ID           string                `json:"id"`
	ResourceKind string                `json:"resource_kind"`
	ResourceUUID string                `json:"resource_uuid"`
	ThreadID     string                `json:"thread_id,omitempty"`
	Anchor       threadBusiness.Anchor `json:"anchor"`
	// BaseHash is the hash of the content the proposal was made against (Base).
	BaseHash string          `json:"base_hash"`
	Base     json.RawMessage `json:"base,omitempty"`
	// Proposed is the whole replacement as codec JSON; Hunks split it for review.
	Proposed json.RawMessage `json:"proposed"`
	Hunks    []Hunk          `json:"hunks"`
	// Note is the proposer's line about it ("Tightened the intro").
	Note       string     `json:"note,omitempty"`
	AuthorKind AuthorKind `json:"author_kind"`
	AuthorUUID string     `json:"author_uuid"`
	AskerUUID  string     `json:"asker_uuid,omitempty"` // for an agent: who asked it
	RunID      string     `json:"run_id,omitempty"`
	Status     Status     `json:"status"`
	// AppliedMode is how an accepted proposal went in: "replace", or
	// "insert_below" when its target had changed.
	AppliedMode string     `json:"applied_mode,omitempty"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// Draft is a change made on the server (an agent's): HTML and where it goes.
	// The kind works out the anchor, base and hunks from the live resource. Never
	// read from a client.
	Draft *Draft `json:"-"`
	// Pending is the step in flight, for the collaboration service only.
	Pending *Pending `json:"-"`
}

// Target is where a server-made change goes in a doc: exactly one of these.
type Target struct {
	BlockIDs     []string `json:"block_ids,omitempty"`      // replace these side-by-side blocks
	AfterBlockID string   `json:"after_block_id,omitempty"` // insert after this block
	Section      string   `json:"section,omitempty"`        // replace what is under this heading
	AfterSection string   `json:"after_section,omitempty"`  // add at the end of this heading's section
	Whole        bool     `json:"whole,omitempty"`          // replace the whole doc
	Append       bool     `json:"append,omitempty"`         // add at the end
}

// Draft is a server-made change.
type Draft struct {
	HTML   string `json:"html"`
	Target Target `json:"target"`
}

// Step modes.
const (
	ModePrepare     = "prepare"
	ModeApply       = "apply"
	ModeInsertBelow = "insert_below"
)

// By is who a step is for, from server context.
type By struct {
	UserUUID  string `json:"user_uuid,omitempty"`
	AgentUUID string `json:"agent_uuid,omitempty"`
}

// Pending is a step the API has asked the collaboration service to run. While it
// is set, nobody else may decide on the proposal.
type Pending struct {
	Mode    string    `json:"mode"`
	HunkIDs []string  `json:"hunk_ids,omitempty"`
	By      By        `json:"by"`
	Target  *Target   `json:"target,omitempty"`
	HTML    string    `json:"html,omitempty"`
	At      time.Time `json:"at"`
}

// Apply says where an accepted change is applied.
const (
	// ApplyServer applies it through the Live Doc Writer: from a list, the phone,
	// the API, without the doc open.
	ApplyServer = "server"
	// ApplyClient records the decision for a reviewer whose editor applies it,
	// so their own ⌘Z undoes it.
	ApplyClient = "client"
)

// Decision is a reviewer's answer: accept the hunks listed (all open ones when
// none are), or decline them.
type Decision struct {
	Accept  bool
	HunkIDs []string
	// Apply is ApplyServer (the default) or ApplyClient.
	Apply string
	// InsertBelow puts the proposed content below the target instead of in its
	// place: what Accept becomes when the target changed.
	InsertBelow bool
}

// Service is the proposals service. Every decision emits a resource event.
type Service interface {
	Create(ctx context.Context, who accessBusiness.Subject, p Proposal) (Proposal, error)
	// List is a resource's proposals, open ones first, then the latest decided.
	List(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) ([]Proposal, error)
	Decide(ctx context.Context, who accessBusiness.Subject, proposalID string, d Decision) (Proposal, error)
	Get(ctx context.Context, who accessBusiness.Subject, proposalID string) (Proposal, error)
	// Withdraw takes an open proposal back: its author, or an editor.
	Withdraw(ctx context.Context, who accessBusiness.Subject, proposalID string) (Proposal, error)
	// Reopen undoes the caller's own decisions on these hunks, within a few
	// minutes: a reviewer whose editor could not apply what they accepted.
	Reopen(ctx context.Context, who accessBusiness.Subject, proposalID string, hunkIDs []string) (Proposal, error)
}

var (
	// ErrUnavailable is a kind of resource with no proposals, or a step the
	// server cannot run yet (the Live Doc Writer before A4).
	ErrUnavailable = errors.New("proposals: not available yet")
	ErrNotFound    = errors.New("proposals: no such proposal")
	ErrForbidden   = errors.New("proposals: not allowed")
	// ErrLocked is a locked resource, which takes no new proposals.
	ErrLocked = errors.New("proposals: the resource is locked")
	// ErrBusy is a proposal another reviewer is deciding on right now.
	ErrBusy    = errors.New("proposals: someone is deciding on it right now")
	ErrTooMany = errors.New("proposals: too many open proposals on this resource")
	ErrDecided = errors.New("proposals: already decided")
)

// InvalidError is a proposal or a decision that cannot be taken as sent.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "proposals: " + e.Reason }

func invalid(format string, a ...any) error { return &InvalidError{Reason: fmt.Sprintf(format, a...)} }

// ConflictError is an accept whose hunks no longer apply: their text changed.
// They can be inserted below or declined.
type ConflictError struct{ HunkIDs []string }

func (e *ConflictError) Error() string {
	return "proposals: the text changed under " + strings.Join(e.HunkIDs, ", ")
}

// Proposals is the service everything asks.
var Proposals Service = defaultService

// defaultService is the service on the proposals table; Collab and the sweep
// use it directly, so a test that swaps Proposals for a fake leaves them be.
var defaultService = &service{store: postgresStore{}, now: time.Now}
