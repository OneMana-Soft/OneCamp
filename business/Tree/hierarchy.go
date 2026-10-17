package business

// The Hierarchy Service for every kind of tree a package registers
// (graph.RegisterKind): docs today; channel folders, calendar lists and
// recording folders later. Generic: a kind says how its nodes are read; this
// orders them, asks the Access Resolver who may see and change each, names
// only what the person may open (I12), and moves nodes with the loop check
// and the tree's lock and stamp.

import (
	"context"
	"errors"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	"github.com/akashc777/OneCamp/business/Tree/position"
	"github.com/akashc777/OneCamp/helpers"
)

// Limits on what one read takes in.
const (
	// levelCap bounds the nodes of one level read at once; a level holding more
	// is listed up to it.
	levelCap = 5000
	// subtreeCap bounds a walk, and what a move into another space takes
	// along: a subtree with more under its page is refused whole (ErrTooMany),
	// never changed part of the way.
	subtreeCap = 5000
	// DefaultPage is a level's page when the caller names none.
	DefaultPage = 50
	// MaxPage is the most one page holds.
	MaxPage = 500
)

// More errors a tree answers with.
var (
	// ErrNotFound is a node that isn't there, or one the person can't open.
	ErrNotFound = errors.New("tree: no such page")
	// ErrForbidden is a change the person may not make.
	ErrForbidden = errors.New("tree: you can't change that page")
	// ErrTooDeep is a move that would nest pages more than MaxAncestors deep.
	ErrTooDeep = graph.ErrTooDeep
	// ErrNotSibling is a page to follow that isn't among the new siblings.
	ErrNotSibling = position.ErrNotSibling
	// ErrBusy is a tree whose lock wasn't free in time.
	ErrBusy = graph.ErrBusy
	// ErrTooMany is a move into another space, or out of one, of a page with
	// more under it than one move reads (subtreeCap): refused, never moved
	// part of the way.
	ErrTooMany = graph.ErrTooMany
	// ErrUnknownKind is a kind of tree nobody registered.
	ErrUnknownKind = errors.New("tree: no such kind of tree")
)

// LevelQuery is one page of one level: a parent's children, or with no
// parent the top level of a space ("" is the kind's first).
type LevelQuery struct {
	Parent string
	Space  string
	Cursor string
	Limit  int
}

// Lister is the tree's fuller reads, beyond the Tree interface: a space's top
// level, a page size, and whether a path was cut short. Hierarchy has it.
type Lister interface {
	Level(ctx context.Context, who accessBusiness.Subject, kind string, q LevelQuery) (Page, error)
	Path(ctx context.Context, who accessBusiness.Subject, kind, uuid string) (nodes []Node, truncated bool, err error)
}

// hierarchy is the Hierarchy Service over the graph.
type hierarchy struct{}

func kindOf(kind string) (graph.Kind, error) {
	k, ok := graph.KindFor(kind)
	if !ok {
		return nil, ErrUnknownKind
	}
	return k, nil
}

func resources(kind string, ids []string) []accessBusiness.Resource {
	out := make([]accessBusiness.Resource, len(ids))
	for i, id := range ids {
		out[i] = accessBusiness.Resource{Kind: kind, UUID: id}
	}
	return out
}

// allowed is which of ids the person has at least level on, as a set.
func allowed(ctx context.Context, who accessBusiness.Subject, kind string, ids []string, level accessBusiness.Level) (map[string]bool, error) {
	kept, err := accessBusiness.Rules.Filter(ctx, who, resources(kind, ids), level)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(kept))
	for _, r := range kept {
		out[r.UUID] = true
	}
	return out, nil
}

// level is the person's level on one node; LevelNone for one that isn't there.
func level(ctx context.Context, who accessBusiness.Subject, kind, id string) (accessBusiness.Level, error) {
	ok, err := allowed(ctx, who, kind, []string{id}, accessBusiness.LevelEdit)
	if err != nil {
		return accessBusiness.LevelNone, err
	}
	if ok[id] {
		return accessBusiness.LevelEdit, nil
	}
	ok, err = allowed(ctx, who, kind, []string{id}, accessBusiness.LevelView)
	if err != nil {
		return accessBusiness.LevelNone, err
	}
	if ok[id] {
		return accessBusiness.LevelView, nil
	}
	return accessBusiness.LevelNone, nil
}

func (h hierarchy) Children(ctx context.Context, who accessBusiness.Subject, kind, parentUUID, cursor string) (Page, error) {
	return h.Level(ctx, who, kind, LevelQuery{Parent: parentUUID, Cursor: cursor})
}

// Level reads one page of a level: the nodes the person may open, in their
// order, each saying whether it has children and whether the person may
// change it; the ones they can't open counted in Hidden, never named.
func (hierarchy) Level(ctx context.Context, who accessBusiness.Subject, kind string, q LevelQuery) (Page, error) {
	k, err := kindOf(kind)
	if err != nil {
		return Page{}, err
	}
	var rows []graph.Row
	if q.Parent == "" {
		rows, err = k.Top(ctx, who.UserUID, q.Space, levelCap)
	} else {
		lv, lerr := level(ctx, who, kind, q.Parent)
		if lerr != nil {
			return Page{}, lerr
		}
		if lv == accessBusiness.LevelNone {
			return Page{}, ErrNotFound
		}
		rows, err = k.Children(ctx, who.UserUID, q.Parent, levelCap)
	}
	if errors.Is(err, graph.ErrNotFound) {
		return Page{}, ErrNotFound
	}
	if err != nil {
		return Page{}, err
	}

	sibs := make([]position.Sibling, len(rows))
	byID := make(map[string]graph.Row, len(rows))
	ids := make([]string, len(rows))
	for i, r := range rows {
		sibs[i], byID[r.UUID], ids[i] = r.Sibling(), r, r.UUID
	}
	position.Order(sibs)
	readable, err := allowed(ctx, who, kind, ids, accessBusiness.LevelView)
	if err != nil {
		return Page{}, err
	}
	var open []string
	for _, s := range sibs {
		if readable[s.ID] {
			open = append(open, s.ID)
		}
	}
	page := Page{Hidden: len(rows) - len(open), Count: len(open)}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultPage
	}
	if limit > MaxPage {
		limit = MaxPage
	}
	from := helpers.CursorOffset(q.Cursor)
	if from > len(open) {
		from = len(open)
	}
	to := from + limit
	if to < len(open) {
		page.Cursor = helpers.OffsetCursor(to)
	} else {
		to = len(open)
	}
	shown := open[from:to]
	editable, err := allowed(ctx, who, kind, shown, accessBusiness.LevelEdit)
	if err != nil {
		return Page{}, err
	}
	page.Nodes = make([]Node, 0, len(shown))
	for _, id := range shown {
		r := byID[id]
		page.Nodes = append(page.Nodes, Node{
			Kind: kind, UUID: r.UUID, ParentUUID: q.Parent, Position: r.Position,
			Title: r.Title, Icon: r.Icon, HasChildren: r.ChildCount > 0, CanEdit: editable[id], Data: r.Data,
		})
	}
	return page, nil
}

func (h hierarchy) Ancestors(ctx context.Context, who accessBusiness.Subject, kind, uuid string) ([]Node, error) {
	nodes, _, err := h.Path(ctx, who, kind, uuid)
	return nodes, err
}

// Path is the pages above a node, the top first, at most MaxAncestors: the
// ones the person can't open come back Redacted, with neither title nor id.
// The person must be able to open the node itself.
func (hierarchy) Path(ctx context.Context, who accessBusiness.Subject, kind, uuid string) ([]Node, bool, error) {
	k, err := kindOf(kind)
	if err != nil {
		return nil, false, err
	}
	lv, err := level(ctx, who, kind, uuid)
	if err != nil {
		return nil, false, err
	}
	if lv == accessBusiness.LevelNone {
		return nil, false, ErrNotFound
	}
	chain, truncated, err := graph.Chain(ctx, k.Schema(), uuid)
	if errors.Is(err, graph.ErrNotFound) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	above := chain[1:]
	if len(above) > MaxAncestors {
		above, truncated = above[:MaxAncestors], true
	}
	if len(above) == 0 {
		return []Node{}, truncated, nil
	}
	ids := make([]string, len(above))
	for i, r := range above {
		ids[i] = r.UUID
	}
	readable, err := allowed(ctx, who, kind, ids, accessBusiness.LevelView)
	if err != nil {
		return nil, false, err
	}
	rows, err := k.Rows(ctx, who.UserUID, ids)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]graph.Row, len(rows))
	for _, r := range rows {
		byID[r.UUID] = r
	}
	// Top first: the chain runs from the node up.
	out := make([]Node, 0, len(above))
	for i := len(above) - 1; i >= 0; i-- {
		id := above[i].UUID
		if !readable[id] {
			out = append(out, Node{Kind: kind, Redacted: true})
			continue
		}
		r := byID[id]
		out = append(out, Node{Kind: kind, UUID: id, Title: r.Title, Icon: r.Icon, Position: r.Position, HasChildren: true})
	}
	return out, truncated, nil
}

// Move moves a page: the person needs edit rights on it and on its new
// parent. ErrCycle for a move under itself, ErrTooDeep past MaxAncestors.
// m.Access is the caller's: a kind applies an adopted parent's sharing itself
// (it knows what sharing is), after the move lands.
//
// A move into another space, or out of one, changes who can open the page
// and every page under it that it takes along, since a space's people reach
// every page in it. That is a change to their sharing, which is full
// access's (A5): the person needs full access to each page whose space
// changes, the ones they can't open included, or the move is
// accessBusiness.ErrSpaceNeedsFull. It is decided here, under the tree's
// lock, from the pages the write then moves. Moved names them. A page with
// more under it than one move reads (subtreeCap) is ErrTooMany: refused,
// never moved part of the way. Taken out of its space to the top, the page is
// placed among the top level it lands in, not its space's.
//
// With m.DryRun every check is made, and nothing is written.
func (hierarchy) Move(ctx context.Context, who accessBusiness.Subject, m Move) (Moved, error) {
	k, err := kindOf(m.Kind)
	if err != nil {
		return Moved{}, err
	}
	s := k.Schema()
	if !m.DryRun {
		unlock, err := graph.Lock(ctx, s.Kind)
		if err != nil {
			return Moved{}, err
		}
		defer unlock()
	}

	ids := []string{m.UUID}
	if m.ParentUUID != "" {
		if m.ParentUUID == m.UUID {
			return Moved{}, ErrCycle
		}
		ids = append(ids, m.ParentUUID)
	}
	editable, err := allowed(ctx, who, m.Kind, ids, accessBusiness.LevelEdit)
	if err != nil {
		return Moved{}, err
	}
	for _, id := range ids {
		if !editable[id] {
			lv, err := level(ctx, who, m.Kind, id)
			if err != nil {
				return Moved{}, err
			}
			if lv == accessBusiness.LevelNone {
				return Moved{}, ErrNotFound
			}
			return Moved{}, ErrForbidden
		}
	}

	// The loop and depth checks read what's there under the lock; the write
	// checks the loop again in its own transaction. One past the cap, so a
	// space move knows a subtree it can't read whole.
	under, height, err := graph.Subtree(ctx, s, m.UUID, subtreeCap+1)
	if errors.Is(err, graph.ErrNotFound) {
		return Moved{}, ErrNotFound
	}
	if err != nil {
		return Moved{}, err
	}
	var moved Moved
	if m.SpaceKind != "" && s.SpaceKind != "" {
		if moved.Spaced, err = spacedBy(m, under, subtreeCap); err != nil {
			return Moved{}, err
		}
		full, err := allowed(ctx, who, m.Kind, moved.Spaced, accessBusiness.LevelFull)
		if err != nil {
			return Moved{}, err
		}
		for _, id := range moved.Spaced {
			if !full[id] {
				return Moved{}, accessBusiness.ErrSpaceNeedsFull
			}
		}
	}
	depth := 0 // levels above the node once moved
	var sibs []position.Sibling
	if m.ParentUUID != "" {
		chain, truncated, err := graph.Chain(ctx, s, m.ParentUUID)
		if errors.Is(err, graph.ErrNotFound) {
			return Moved{}, ErrNotFound
		}
		if err != nil {
			return Moved{}, err
		}
		for _, r := range chain {
			if r.UUID == m.UUID {
				return Moved{}, ErrCycle
			}
		}
		if truncated {
			return Moved{}, ErrTooDeep
		}
		depth = len(chain)
		sibs, err = graph.Siblings(ctx, s, m.ParentUUID, levelCap)
		if err != nil {
			return Moved{}, err
		}
	} else {
		var rows []graph.Row
		if m.SpaceKind != "" && m.SpaceKind != SpaceNone {
			rows, err = k.Top(ctx, who.UserUID, m.SpaceKind+":"+m.SpaceUUID, levelCap)
		} else {
			rows, err = k.TopFor(ctx, who.UserUID, m.UUID, m.SpaceKind == SpaceNone, levelCap)
		}
		if err != nil {
			return Moved{}, err
		}
		for _, r := range rows {
			sibs = append(sibs, r.Sibling())
		}
		position.Order(sibs)
	}
	if depth+1+height > MaxAncestors {
		return Moved{}, ErrTooDeep
	}
	where := position.Where{After: m.AfterUUID, First: m.AfterUUID == "" && m.Place != PlaceLast}
	placed, err := position.Place(sibs, m.UUID, where)
	if err != nil {
		return Moved{}, err
	}
	if m.DryRun {
		return moved, nil
	}
	move := graph.Move{UUID: m.UUID, Parent: m.ParentUUID, Key: placed.Key, Renumber: placed.Renumber}
	if m.SpaceKind != "" && s.SpaceKind != "" {
		move.Set = map[string]string{s.SpaceKind: m.SpaceKind, s.SpaceUUID: m.SpaceUUID}
		if m.SpaceKind == SpaceNone {
			move.Set = map[string]string{s.SpaceKind: "", s.SpaceUUID: ""}
		}
		for _, r := range under {
			move.Under = append(move.Under, r.UID)
		}
	}
	err = graph.Apply(ctx, s, move, time.Now())
	switch {
	case errors.Is(err, graph.ErrCycle):
		return Moved{}, ErrCycle
	case errors.Is(err, graph.ErrNotFound):
		return Moved{}, ErrNotFound
	case err != nil:
		return Moved{}, err
	}
	return moved, nil
}

// spacedBy is the pages a move into another space, or out of one, changes:
// the page, which the caller names a space for only when it changes, and each
// page under it that isn't in that space already. under holding more than
// limit pages is a subtree larger than one move reads: ErrTooMany, so no page
// under it changes space unchecked. Pure.
func spacedBy(m Move, under []graph.Ref, limit int) ([]string, error) {
	if len(under) > limit {
		return nil, ErrTooMany
	}
	kind, id := m.SpaceKind, m.SpaceUUID
	if kind == SpaceNone {
		kind, id = "", ""
	}
	out := []string{m.UUID}
	for _, r := range under {
		if r.SpaceKind != kind || r.SpaceUUID != id {
			out = append(out, r.UUID)
		}
	}
	return out, nil
}

// Walk visits a node and everything under it, the node first, parents before
// their children, naming nothing (for the server's own work). A node with more
// than subtreeCap pages under it is ErrTooMany before any is visited: what is
// done over a subtree (a sharing change applied to the pages under one, a page
// that takes its new parent's sharing, a copy) takes all of it or none, as a
// delete (business/Doc TrashSubtree) and a move into another space do.
func (hierarchy) Walk(ctx context.Context, kind, rootUUID string, visit func(Node) error) error {
	k, err := kindOf(kind)
	if err != nil {
		return err
	}
	// One past the cap, so a subtree it can't read whole is known.
	under, _, err := graph.Subtree(ctx, k.Schema(), rootUUID, subtreeCap+1)
	if errors.Is(err, graph.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(under) > subtreeCap {
		return ErrTooMany
	}
	if err := visit(Node{Kind: kind, UUID: rootUUID}); err != nil {
		return err
	}
	for _, r := range under {
		if err := visit(Node{Kind: kind, UUID: r.UUID, ParentUUID: r.ParentUUID}); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	// The Access Resolver asks the tree what is under a page (ApplyToSubtree);
	// it can't import this package, which imports it.
	accessBusiness.Subtree = func(ctx context.Context, root accessBusiness.Resource) ([]accessBusiness.Resource, error) {
		var out []accessBusiness.Resource
		err := Hierarchy.Walk(ctx, root.Kind, root.UUID, func(n Node) error {
			out = append(out, accessBusiness.Resource{Kind: n.Kind, UUID: n.UUID})
			return nil
		})
		if errors.Is(err, ErrUnknownKind) {
			return []accessBusiness.Resource{root}, nil
		}
		return out, err
	}
}
