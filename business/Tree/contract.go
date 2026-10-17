package business

// The Hierarchy Service: pages under pages, generic over kinds (docs programme,
// PROGRAM.md §4 #7; A3 builds it). A parent, a fractional position among
// siblings, children with has_children, ancestors capped at MaxAncestors, moves
// with a cycle check, subtree walks, and redaction: a list, tree or breadcrumb
// never carries the title of a page the viewer can't open (I12).
//
// A kind of tree registers its reads in business/Tree/graph (docs do, from
// business/Doc); hierarchy.go orders them, asks the Access Resolver who may see
// and change what, and moves pages under the tree's lock and stamp. Positions
// are business/Tree/position's fractional keys.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// MaxAncestors is the deepest a breadcrumb reaches, and how deep pages nest.
const MaxAncestors = 32

// Node is one page in a tree.
type Node struct {
	Kind        string `json:"kind"`
	UUID        string `json:"uuid,omitempty"` // empty when Redacted
	ParentUUID  string `json:"parent_uuid,omitempty"`
	Position    string `json:"position,omitempty"` // a fractional key among its siblings
	Title       string `json:"title,omitempty"`    // empty when Redacted, or when nobody named it yet
	Icon        string `json:"icon,omitempty"`
	HasChildren bool   `json:"has_children"`
	// CanEdit is whether the viewer may change the page: rename it, move it,
	// put pages in it.
	CanEdit bool `json:"can_edit,omitempty"`
	// Redacted is a page the viewer can't open: it is counted or shown as a gap,
	// never titled (I12).
	Redacted bool `json:"redacted,omitempty"`
	// Data is the kind's own row (a doc), for an answer that shows more of a
	// page than the tree does. Never sent as it is.
	Data any `json:"-"`
}

// Page is one page of a node's children.
type Page struct {
	Nodes []Node `json:"nodes"`
	// Hidden counts the children the viewer can't open ("2 pages you can't open").
	Hidden int `json:"hidden"`
	// Count is how many children the viewer can open, on this page and after it.
	Count  int    `json:"count"`
	Cursor string `json:"cursor,omitempty"`
}

// AccessChoice is what a move does to a page's sharing.
type AccessChoice string

const (
	// KeepAccess keeps the page's own sharing.
	KeepAccess AccessChoice = "keep"
	// AdoptAccess gives it its new parent's.
	AdoptAccess AccessChoice = "adopt"
)

// Where a moved page goes among its new siblings when no AfterUUID is named.
const (
	PlaceFirst = "first"
	PlaceLast  = "last"
)

// SpaceNone, as a move's SpaceKind, takes a page out of any space.
const SpaceNone = "none"

// Move puts a page somewhere else in its tree.
type Move struct {
	Kind       string
	UUID       string
	ParentUUID string // "" moves it to the top
	AfterUUID  string // the sibling it goes right after; "" puts it first (or last: Place)
	// Place is PlaceLast to put it last among its new siblings when AfterUUID
	// is ""; anything else puts it first.
	Place string
	// SpaceKind and SpaceUUID put the page, and everything under it, in a
	// space: "" keeps the one it has, SpaceNone takes it out of any. Name one
	// only for a move that changes the page's space.
	SpaceKind string
	SpaceUUID string
	// Access is the caller's to apply: a kind knows what its sharing is, and
	// adopts the new parent's once the move has landed.
	Access AccessChoice
	// DryRun checks the move and says what it would do, without making it:
	// a preview, which refuses exactly what the move would.
	DryRun bool
}

// Moved is what a move did, or with DryRun would do, that its kind follows
// up on: the pages whose space it changed, the page first, none when it kept
// the page's space. Who can open those changed (a space's people reach every
// page in it), so the kind does what follows a change to their sharing.
type Moved struct {
	Spaced []string
}

var (
	// ErrCycle is a move that would put a page under itself.
	ErrCycle = errors.New("tree: a page can't move under itself")
	// ErrUnavailable is an operation a tree doesn't have.
	ErrUnavailable = errors.New("tree: not available yet")
)

// Tree is the Hierarchy Service.
type Tree interface {
	// Children is one level under parentUUID, in position order. "" is the top.
	Children(ctx context.Context, who accessBusiness.Subject, kind, parentUUID, cursor string) (Page, error)
	// Ancestors is the path from the top down to the page's parent, at most
	// MaxAncestors long; the ones the viewer can't open come back Redacted.
	Ancestors(ctx context.Context, who accessBusiness.Subject, kind, uuid string) ([]Node, error)
	// Move moves a page. It needs edit rights at both ends, and answers ErrCycle
	// for a move under itself. A move into another space, or out of one, also
	// needs full access to every page whose space it changes.
	Move(ctx context.Context, who accessBusiness.Subject, m Move) (Moved, error)
	// Walk visits a page and everything under it, the page first. It is for the
	// server's own work (a sharing change to the pages under one, a duplicate)
	// and redacts nothing. A page with more under it than one walk reads is
	// ErrTooMany, before any page is visited.
	Walk(ctx context.Context, kind, rootUUID string, visit func(Node) error) error
}

// Hierarchy is the tree everything asks.
var Hierarchy Tree = hierarchy{}
