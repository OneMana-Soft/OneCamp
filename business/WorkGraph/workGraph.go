// Package workGraphBusiness is the work graph for one reader (DG5): what is
// connected to a thing, one or two hops out, with the path that connects it,
// and the shortest ways two things are connected.
//
// HOW IT DECIDES WHAT A READER SEES. domain/WorkGraph reads the graph over
// its allow-list of edges, live nodes only, capped at every hop. Every node
// that comes back, and the thing asked about, then goes through
// business/EntityLink ReadMany, as the reader: the one rule every chip,
// title and link in the app is decided by (the Access Resolver for docs and
// channels, the task view's rule for tasks and projects, and each kind's
// own). Then:
//
//   - a thing the reader can't see is "not found", the same answer as a
//     thing that isn't there;
//   - a path through anything the reader can't see is dropped whole. A node
//     reached only through something hidden isn't shown, and no gap is: a
//     gap would tell the reader that something they can't see is there;
//   - nothing counts what was dropped. "More" says only that more of what
//     the reader can see was found than a group shows.
//
// It reads uuids and kinds, never a graph uid from a client.
package workGraphBusiness

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
)

// ErrNotFound is a thing that isn't there or that the reader may not see.
var ErrNotFound = errors.New("work graph: not found")

// ErrBadRequest is a kind, depth or id the work graph doesn't take.
var ErrBadRequest = errors.New("work graph: bad request")

const (
	// PerKind is how many things a group shows.
	PerKind = 12
	// MaxCandidates bounds the nodes one read decides: the nearest first.
	MaxCandidates = 300
)

// Ref names one thing by its kind and uuid.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Hop is one step of a path, as a reader may see it: the relationship from
// the previous thing's side, and the thing it reaches.
type Hop struct {
	Rel   string         `json:"rel"`
	Kind  string         `json:"kind"`
	ID    string         `json:"id"`
	Title string         `json:"title"`
	Data  map[string]any `json:"data,omitempty"`
}

// Item is a thing connected to the root, with the path that connects it
// (its last hop is the thing itself).
type Item struct {
	Hop
	Depth int   `json:"depth"`
	Path  []Hop `json:"path"`
}

// Group is the things of one kind.
type Group struct {
	Kind  string `json:"kind"`
	Items []Item `json:"items"`
	More  bool   `json:"more"`
}

// Connected is what is connected to a thing.
type Connected struct {
	Root   Hop     `json:"root"`
	Depth  int     `json:"depth"`
	Groups []Group `json:"groups"`
}

// Path is one way from a thing to another.
type Path struct {
	Hops []Hop `json:"hops"`
}

// Paths is how two things are connected.
type Paths struct {
	From  Hop    `json:"from"`
	To    Hop    `json:"to"`
	Paths []Path `json:"paths"`
}

// Where the graph and the readers are read (seams for tests).
var (
	readNeighbours = domain.Neighbours
	readPaths      = domain.Paths
	readMany       = entityLinkBusiness.ReadMany
)

// ParseKinds is a kinds filter as a request writes it ("doc,task"): each a
// kind of the work graph; the panel's kinds when empty.
func ParseKinds(s string) ([]domain.Kind, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return domain.DefaultKinds, nil
	}
	var out []domain.Kind
	for _, part := range strings.Split(s, ",") {
		k := domain.Kind(strings.TrimSpace(part))
		if !domain.Known(k) {
			return nil, ErrBadRequest
		}
		out = append(out, k)
	}
	return out, nil
}

// check is a ref the work graph takes: a known kind and a uuid.
func check(r Ref) (domain.Ref, error) {
	k := domain.Kind(r.Kind)
	if !domain.Known(k) || !helpers.IsCanonicalUUID(r.ID) {
		return domain.Ref{}, ErrBadRequest
	}
	return domain.Ref{Kind: k, UUID: strings.ToLower(r.ID)}, nil
}

func refOf(r domain.Ref) entityLinkBusiness.Ref {
	return entityLinkBusiness.Ref{Kind: string(r.Kind), ID: r.UUID}
}

// decide reads every ref as each of readers sees it, one ReadMany call per
// reader: the refs every one of them may see, named as the first reader
// sees them. With two readers (an agent's sponsor and the person who asked
// it, or a token's owner and the asker) a thing either of them can't see is
// left out, so a path through it is dropped whole, as for one reader.
func decide(ctx context.Context, readers []accessBusiness.Subject, refs []domain.Ref) (map[domain.Ref]entityLinkBusiness.Entity, error) {
	asked := make([]entityLinkBusiness.Ref, 0, len(refs))
	for _, r := range refs {
		asked = append(asked, refOf(r))
	}
	var out map[domain.Ref]entityLinkBusiness.Entity
	for n, who := range readers {
		got, err := readMany(ctx, who, asked)
		if err != nil {
			return nil, err
		}
		seen := make(map[domain.Ref]entityLinkBusiness.Entity, len(got))
		for i, e := range got {
			if i < len(refs) && e.Access == entityLinkBusiness.AccessOK {
				seen[refs[i]] = e
			}
		}
		if n == 0 {
			out = seen
			continue
		}
		for r := range out {
			if _, ok := seen[r]; !ok {
				delete(out, r)
			}
		}
	}
	return out, nil
}

// anyone reports whether readers names at least one person, each of them
// someone: a read for no one, or for someone unnamed, sees nothing.
func anyone(readers []accessBusiness.Subject) bool {
	for _, who := range readers {
		if who.UserUUID == "" {
			return false
		}
	}
	return len(readers) > 0
}

func hopOf(rel string, r domain.Ref, e entityLinkBusiness.Entity) Hop {
	return Hop{Rel: rel, Kind: string(r.Kind), ID: r.UUID, Title: e.Title, Data: e.Data}
}

// NeighboursOf is what is connected to root, up to depth (1 or 2) hops, of
// kinds (the panel's kinds when nil), as who may see it, grouped by kind.
func NeighboursOf(ctx context.Context, who accessBusiness.Subject, root Ref, depth int, kinds []domain.Kind) (*Connected, error) {
	return NeighboursFor(ctx, []accessBusiness.Subject{who}, root, depth, kinds)
}

// NeighboursFor is NeighboursOf for several readers at once: the root, and
// every thing on a path, must be one each of them may see. What an agent
// tells the person who asked it, when it runs as someone else (its sponsor,
// a token's owner), is what both of them may see. Still one graph read, and
// one ReadMany per reader.
func NeighboursFor(ctx context.Context, readers []accessBusiness.Subject, root Ref, depth int, kinds []domain.Kind) (*Connected, error) {
	r, err := check(root)
	if err != nil || (depth != 1 && depth != 2) {
		return nil, ErrBadRequest
	}
	if !anyone(readers) {
		return nil, ErrNotFound
	}
	if len(kinds) == 0 {
		kinds = domain.DefaultKinds
	}
	reached, err := readNeighbours(ctx, r, depth, kinds)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// The nodes to decide: the root, then every node on the nearest paths,
	// until MaxCandidates. A path whose nodes don't all fit is left out.
	refs := []domain.Ref{r}
	in := map[domain.Ref]bool{r: true}
	var kept []domain.Reached
	for _, p := range reached {
		var add []domain.Ref
		for _, h := range p.Path {
			if !in[h.To] && !hasRef(add, h.To) {
				add = append(add, h.To)
			}
		}
		if len(refs)+len(add) > MaxCandidates {
			continue
		}
		for _, a := range add {
			in[a] = true
			refs = append(refs, a)
		}
		kept = append(kept, p)
	}
	seen, err := decide(ctx, readers, refs)
	if err != nil {
		return nil, err
	}
	rootEntity, ok := seen[r]
	if !ok {
		return nil, ErrNotFound
	}
	out := &Connected{Root: hopOf("", r, rootEntity), Depth: depth}
	groups := map[domain.Kind]*Group{}
	shown := map[domain.Ref]bool{}
	for _, p := range kept {
		if shown[p.Node] || !shows(kinds, p) {
			continue
		}
		path, ok := visiblePath(p.Path, seen)
		if !ok {
			// Through something the reader can't see: dropped whole.
			continue
		}
		shown[p.Node] = true
		g := groups[p.Node.Kind]
		if g == nil {
			g = &Group{Kind: string(p.Node.Kind)}
			groups[p.Node.Kind] = g
		}
		if len(g.Items) == PerKind {
			g.More = true
			continue
		}
		g.Items = append(g.Items, Item{Hop: path[len(path)-1], Depth: len(path), Path: path})
	}
	for _, k := range kindOrder(kinds) {
		if g := groups[k]; g != nil {
			out.Groups = append(out.Groups, *g)
		}
	}
	return out, nil
}

// visiblePath is a path as the reader sees it, or false when any thing on
// it is hidden from them.
func visiblePath(hops []domain.Hop, seen map[domain.Ref]entityLinkBusiness.Entity) ([]Hop, bool) {
	out := make([]Hop, 0, len(hops))
	for _, h := range hops {
		e, ok := seen[h.To]
		if !ok {
			return nil, false
		}
		out = append(out, hopOf(h.Rel, h.To, e))
	}
	return out, len(out) > 0
}

// PathsBetween is at most three shortest paths from a to b, as who may see
// them: ErrNotFound when either is hidden from them or isn't there, and none
// when every path found goes through something hidden.
func PathsBetween(ctx context.Context, who accessBusiness.Subject, a, b Ref) (*Paths, error) {
	return PathsFor(ctx, []accessBusiness.Subject{who}, a, b)
}

// PathsFor is PathsBetween for several readers at once: both ends, and every
// thing on a path, must be one each of them may see (see NeighboursFor).
func PathsFor(ctx context.Context, readers []accessBusiness.Subject, a, b Ref) (*Paths, error) {
	ra, err := check(a)
	if err != nil {
		return nil, err
	}
	rb, err := check(b)
	if err != nil {
		return nil, err
	}
	if !anyone(readers) {
		return nil, ErrNotFound
	}
	found, err := readPaths(ctx, ra, rb)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	refs := []domain.Ref{ra, rb}
	for _, p := range found {
		for _, h := range p.Hops {
			if !hasRef(refs, h.To) {
				refs = append(refs, h.To)
			}
		}
	}
	seen, err := decide(ctx, readers, refs)
	if err != nil {
		return nil, err
	}
	ea, okA := seen[ra]
	eb, okB := seen[rb]
	if !okA || !okB {
		return nil, ErrNotFound
	}
	out := &Paths{From: hopOf("", ra, ea), To: hopOf("", rb, eb), Paths: []Path{}}
	for _, p := range found {
		if hops, ok := visiblePath(p.Hops, seen); ok {
			out.Paths = append(out.Paths, Path{Hops: hops})
		}
	}
	return out, nil
}

func hasRef(rs []domain.Ref, r domain.Ref) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// shows is whether a reached node goes in the answer: its kind was asked
// for, or it is a comment that links the thing it was reached from (DG6)
// while messages are asked for. A comment merely on a thing is shown only
// when comments are asked for by name; one whose body links here is a
// reference, as a message's is, and goes where messages go.
func shows(kinds []domain.Kind, p domain.Reached) bool {
	if wanted(kinds, p.Node.Kind) {
		return true
	}
	last := len(p.Path) - 1
	return p.Node.Kind == domain.KindComment && last >= 0 && p.Path[last].Pred == "~"+domain.RefersTo &&
		(wanted(kinds, domain.KindMessage) || wanted(kinds, domain.KindChat))
}

func wanted(kinds []domain.Kind, k domain.Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// kindOrder is the order groups come in: the panel's, then any other asked,
// then comments (shown when they link here, asked for or not).
func kindOrder(kinds []domain.Kind) []domain.Kind {
	out := append([]domain.Kind(nil), domain.DefaultKinds...)
	for _, k := range append(append([]domain.Kind(nil), kinds...), domain.KindComment) {
		if !wanted(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// Link is one relationship from a thing the reader already has (a search
// hit) to a thing one hop from it, as every reader may see both.
type Link struct {
	From Hop       `json:"from"`
	Rel  string    `json:"rel"`
	To   Hop       `json:"to"`
	When time.Time `json:"when,omitempty"`
}

// ExpandMaxLinks is the most links one Expand gives.
const ExpandMaxLinks = 30

// expandPerEdge is how many targets of each edge are read from each seed.
const expandPerEdge = 4

// readExpand is the graph's one-hop read from many things (a seam for tests).
var readExpand = domain.Expand

// expandKindRank is the order links are kept in when there are more than
// ExpandMaxLinks: what work hangs on first (tasks, projects, docs), then who,
// then where it was said, then the rest.
var expandKindRank = map[domain.Kind]int{
	domain.KindTask: 0, domain.KindProject: 1, domain.KindDoc: 2, domain.KindPerson: 3,
	domain.KindMessage: 4, domain.KindChannel: 5, domain.KindBoard: 6, domain.KindTeam: 7,
}

// ExpandFor is what is one hop from seeds (search hits the readers may
// already see), of kinds (the panel's kinds when nil), as every one of
// readers may see it: one graph read, then one ReadMany per reader for the
// seeds and everything they reach together. A seed any reader can't see is
// dropped with everything it reaches, and so is a link to anything any of
// them can't see. At most ExpandMaxLinks links, ranked by kind and then by
// the most recent change, a seed's own order breaking ties; no thing is the
// end of two links. Two seeds can be linked: that a hit is linked from
// another hit is worth saying.
func ExpandFor(ctx context.Context, readers []accessBusiness.Subject, seeds []Ref, kinds []domain.Kind) ([]Link, error) {
	if !anyone(readers) {
		return nil, ErrNotFound
	}
	if len(kinds) == 0 {
		kinds = domain.DefaultKinds
	}
	var roots []domain.Ref
	for _, s := range seeds {
		r, err := check(s)
		if err != nil {
			continue // a hit the graph doesn't hold (a DM, a memory note) adds nothing
		}
		if !hasRef(roots, r) && len(roots) < domain.ExpandMaxRoots {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	near, err := readExpand(ctx, roots, kinds, expandPerEdge)
	if err != nil {
		return nil, err
	}
	refs := append([]domain.Ref(nil), roots...)
	for _, n := range near {
		if !hasRef(refs, n.Node) {
			refs = append(refs, n.Node)
		}
	}
	seen, err := decide(ctx, readers, refs)
	if err != nil {
		return nil, err
	}
	type cand struct {
		link Link
		kind domain.Kind
		seed int
	}
	var cands []cand
	given := map[domain.Ref]bool{}
	for _, n := range near {
		from, okF := seen[n.Root]
		to, okT := seen[n.Node]
		if !okF || !okT || given[n.Node] || !wanted(kinds, n.Node.Kind) {
			continue
		}
		given[n.Node] = true
		seed := 0
		for i, r := range roots {
			if r == n.Root {
				seed = i
			}
		}
		cands = append(cands, cand{
			link: Link{From: hopOf("", n.Root, from), Rel: n.Rel, To: hopOf(n.Rel, n.Node, to), When: n.Changed},
			kind: n.Node.Kind, seed: seed,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		ra, rb := kindRank(a.kind), kindRank(b.kind)
		if ra != rb {
			return ra < rb
		}
		if !a.link.When.Equal(b.link.When) {
			return a.link.When.After(b.link.When)
		}
		return a.seed < b.seed
	})
	if len(cands) > ExpandMaxLinks {
		cands = cands[:ExpandMaxLinks]
	}
	out := make([]Link, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.link)
	}
	return out, nil
}

func kindRank(k domain.Kind) int {
	if r, ok := expandKindRank[k]; ok {
		return r
	}
	return len(expandKindRank)
}
