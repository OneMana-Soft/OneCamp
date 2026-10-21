package business

// The Snapshot Service: versions of any kind of resource (docs programme,
// PROGRAM.md §4 #10; A4 built it). Tiered retention within a byte cap, named and
// pinned versions that pruning keeps, a contributor ledger that records who edited
// between two versions (an agent with the person who asked it), preview, a restore
// that applies live for everyone, and agent_run versions that an agent's Revert
// goes back to.
//
// GENERIC. The service holds no rules about docs: each kind of resource registers
// a Resource (Register), which says who may see and change its versions, keeps
// them, and applies one back. Docs are the first (docVersions.go). Message edit
// history, calendar event history and recap versions register their own, and get
// the same API, ledger and retention (retention.go, ledger.go).
//
// Code that needs a version asks Snapshots:
//
//	v, err := snapshotBusiness.Snapshots.Take(ctx, snapshotBusiness.Take{ResourceKind: "doc", ResourceUUID: id, Name: "Before the import"})
//
// and a test that needs a different answer sets Snapshots to a fake and puts it
// back.

import (
	"context"
	"errors"
	"sync"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Kind is what a version was taken of.
type Kind string

const (
	// KindDoc is a version of a doc's body.
	KindDoc Kind = "doc"
	// KindAgentRun is the body before an agent's run, which Revert restores.
	KindAgentRun Kind = "agent_run"
)

// Contributor is someone whose edits a version holds. For an agent's edit,
// AgentUUID is the agent and AskerUUID the person who asked it (I11).
type Contributor struct {
	UserUUID  string `json:"user_uuid,omitempty"`
	AgentUUID string `json:"agent_uuid,omitempty"`
	AskerUUID string `json:"asker_uuid,omitempty"`
}

// Version is one stored version.
type Version struct {
	ID           string        `json:"id"`
	ResourceKind string        `json:"resource_kind"`
	ResourceUUID string        `json:"resource_uuid"`
	Kind         Kind          `json:"kind"`
	Name         string        `json:"name,omitempty"`
	Pinned       bool          `json:"pinned"`
	Lineage      string        `json:"lineage,omitempty"`
	SizeBytes    int           `json:"size_bytes"`
	Reason       string        `json:"reason,omitempty"`
	RunID        string        `json:"run_id,omitempty"`
	Contributors []Contributor `json:"contributors"`
	CreatedAt    time.Time     `json:"created_at"`
}

// Take asks for a version now.
type Take struct {
	ResourceKind string
	ResourceUUID string
	Kind         Kind
	Name         string // a named version is kept past pruning
	Pinned       bool
	// Reason is why (the doc reasons: "saved" when a person keeps one).
	Reason string
	By     Contributor
	RunID  string // for KindAgentRun
	// Body, when set, is the content to keep; otherwise the resource's
	// content now is read (for a doc, the live copy, typing included).
	Body    string
	Lineage string
	// Who, when set, must be allowed to change the resource's versions; a
	// version the server takes on its own leaves it nil.
	Who *accessBusiness.Subject
}

// Service is the Snapshot Service.
type Service interface {
	Take(ctx context.Context, t Take) (Version, error)
	// List is the versions newest first, a page at a time: those before
	// `before` when it is set.
	List(ctx context.Context, who accessBusiness.Subject, resourceKind, resourceUUID string, before time.Time) ([]Version, error)
	// Preview is a version's body as HTML, for reading beside the live doc.
	Preview(ctx context.Context, who accessBusiness.Subject, resourceKind, resourceUUID, versionID string) (string, error)
	// Name names a version and pins or unpins it ("" unnames it).
	Name(ctx context.Context, who accessBusiness.Subject, resourceKind, resourceUUID, versionID, name string, pinned bool) error
	// Restore makes a version the current body, live for everyone.
	Restore(ctx context.Context, who accessBusiness.Subject, resourceKind, resourceUUID, versionID string) error
}

var (
	// ErrUnknownKind is a resource kind with no versions.
	ErrUnknownKind = errors.New("snapshot: no versions for this kind of resource")
	// ErrNotFound is no such resource, or no such version of it.
	ErrNotFound = errors.New("snapshot: not found")
	// ErrForbidden is someone who may not see or change the versions.
	ErrForbidden = errors.New("snapshot: not allowed")
	// ErrInTrash is a resource in the trash: its history waits there with it.
	ErrInTrash = errors.New("snapshot: the resource is in the trash")
	// ErrKeptFull is a name or pin that would take the resource's kept
	// versions past their byte cap.
	ErrKeptFull = errors.New("snapshot: there is no room to keep another version of this")
	// ErrBadName is a version name that is too long.
	ErrBadName = errors.New("snapshot: a version's name is at most 120 characters")
)

// Resource is how the service reaches one kind of resource.
type Resource interface {
	// Allow answers nil when who may see and change the resource's versions,
	// else ErrNotFound, ErrForbidden or ErrInTrash.
	Allow(ctx context.Context, who accessBusiness.Subject, resourceUUID string) error
	Take(ctx context.Context, t Take) (Version, error)
	List(ctx context.Context, resourceUUID string, before time.Time) ([]Version, error)
	Preview(ctx context.Context, resourceUUID, versionID string) (string, error)
	Name(ctx context.Context, resourceUUID, versionID, name string, pinned bool) error
	Restore(ctx context.Context, who accessBusiness.Subject, resourceUUID, versionID string) error
}

var (
	resourcesMu sync.RWMutex
	resources   = map[string]Resource{}
)

// Register makes a kind of resource's versions reachable through Snapshots.
// Twice under one kind panics: registration happens in init, where that fails
// the build's tests.
func Register(resourceKind string, r Resource) {
	resourcesMu.Lock()
	defer resourcesMu.Unlock()
	if _, taken := resources[resourceKind]; taken || r == nil || resourceKind == "" {
		panic("snapshot: resource kind " + resourceKind + " registered twice or empty")
	}
	resources[resourceKind] = r
}

func resourceOf(kind string) (Resource, error) {
	resourcesMu.RLock()
	defer resourcesMu.RUnlock()
	r, ok := resources[kind]
	if !ok {
		return nil, ErrUnknownKind
	}
	return r, nil
}

// Snapshots is the service everything asks.
var Snapshots Service = versions{}

// versions dispatches to each kind's Resource, checking access first.
type versions struct{}

func (versions) Take(ctx context.Context, t Take) (Version, error) {
	r, err := resourceOf(t.ResourceKind)
	if err != nil {
		return Version{}, err
	}
	if t.Who != nil {
		if err := r.Allow(ctx, *t.Who, t.ResourceUUID); err != nil {
			return Version{}, err
		}
	}
	if len(t.Name) > 120 {
		return Version{}, ErrBadName
	}
	return r.Take(ctx, t)
}

func (versions) List(ctx context.Context, who accessBusiness.Subject, kind, uuid string, before time.Time) ([]Version, error) {
	r, err := resourceOf(kind)
	if err != nil {
		return nil, err
	}
	if err := r.Allow(ctx, who, uuid); err != nil {
		return nil, err
	}
	return r.List(ctx, uuid, before)
}

func (versions) Preview(ctx context.Context, who accessBusiness.Subject, kind, uuid, versionID string) (string, error) {
	r, err := resourceOf(kind)
	if err != nil {
		return "", err
	}
	if err := r.Allow(ctx, who, uuid); err != nil {
		return "", err
	}
	return r.Preview(ctx, uuid, versionID)
}

func (versions) Name(ctx context.Context, who accessBusiness.Subject, kind, uuid, versionID, name string, pinned bool) error {
	r, err := resourceOf(kind)
	if err != nil {
		return err
	}
	if len([]rune(name)) > 120 {
		return ErrBadName
	}
	if err := r.Allow(ctx, who, uuid); err != nil {
		return err
	}
	return r.Name(ctx, uuid, versionID, name, pinned)
}

func (versions) Restore(ctx context.Context, who accessBusiness.Subject, kind, uuid, versionID string) error {
	r, err := resourceOf(kind)
	if err != nil {
		return err
	}
	if err := r.Allow(ctx, who, uuid); err != nil {
		return err
	}
	return r.Restore(ctx, who, uuid, versionID)
}
