// Package graph keeps trees in Dgraph, for any kind of thing: a node's parent
// is one uid edge with @reverse (so a level is its parent's reverse edge) and
// its place among siblings a position key (business/Tree/position). It reads
// structure (uids, ids, positions) and writes moves; what a kind shows of a
// node, and who may see it, is the kind's and business/Tree's.
//
// Moves are upserts that check, in the transaction that writes, that the new
// parent isn't the node or under it, and every change to a tree's shape writes
// the tree's stamp: Dgraph only notices two transactions colliding when they
// write the same thing, and two moves that would close a loop together (A under
// B while B goes under A) write different nodes. With the stamp in both, the
// second to commit is aborted and run again (dgraphInit.DoCommitNow), sees the
// first, and finds the loop. The lock (lock.go) keeps changes from colliding in
// the first place; the stamp still holds when the lock can't be had.
//
// A package of its own, below business/Tree, so the kinds' own packages
// (business/Doc) can use it: business/Tree depends on business/Access, which
// depends on business/Doc.
package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/business/Tree/position"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// MaxDepth is how deep a tree nests, and how far up a walk goes: business/Tree's
// MaxAncestors.
const MaxDepth = 32

// Schema names one kind's predicates. Every name is written into queries, so a
// Schema is a constant of the code, never input.
type Schema struct {
	// Kind names the tree: its lock, its stamp, its registry entry ("doc").
	Kind string
	// ID is a node's id ("doc_uuid"); Parent its parent edge, a uid with
	// @reverse ("doc_parent"); Position its key ("doc_position"); Created when
	// it was made ("doc_created_at"), the order of nodes with no key yet.
	ID, Parent, Position, Created string
	// SpaceKind and SpaceUUID name the space a node is in, which a subtree
	// moved into another space follows; empty for a kind without spaces.
	SpaceKind, SpaceUUID string
	// Live is a filter keeping the nodes in the tree (not deleted). A node that
	// isn't live is nobody's child and nobody's parent here.
	Live string
	// Leaf is a filter that holds of a node that holds no others: a doc
	// tree's file or link (docs F1, D-30). "" for a kind whose every node may
	// have children. It filters one node read by its id, never a root.
	Leaf string
}

var predicateName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (s Schema) valid() bool {
	for _, p := range []string{s.Kind, s.ID, s.Parent, s.Position, s.Created} {
		if !predicateName.MatchString(p) {
			return false
		}
	}
	for _, p := range []string{s.SpaceKind, s.SpaceUUID} {
		if p != "" && !predicateName.MatchString(p) {
			return false
		}
	}
	return s.Live != ""
}

// Errors the graph answers with.
var (
	ErrCycle     = errors.New("graph: a node can't go under itself or one of its own subpages")
	ErrTooDeep   = fmt.Errorf("graph: that would nest more than %d levels deep", MaxDepth)
	ErrNotFound  = errors.New("graph: no such node in the tree")
	ErrBadSchema = errors.New("graph: a schema or a move names something that isn't a predicate or a uid")
	// ErrTooMany is a subtree too large to change at once: refused whole,
	// never changed part of the way.
	ErrTooMany = errors.New("graph: too many pages under this one to change at once")
	// ErrLeaf is a node that holds no others (Schema.Leaf) named as a new
	// parent.
	ErrLeaf = errors.New("graph: that node can't hold others")
)

// Row is a node as a kind reads it for one person.
type Row struct {
	UID        string
	UUID       string
	Title      string
	Icon       string
	Position   string
	CreatedAt  time.Time
	ChildCount int
	// Data is the kind's own row (a doc), for answers that show more of a node
	// than the tree does.
	Data any
}

// Sibling is the row as its level is ordered.
func (r Row) Sibling() position.Sibling {
	return position.Sibling{ID: r.UUID, Position: r.Position, CreatedAt: r.CreatedAt}
}

// Kind is one kind of tree's reads, for business/Tree. A kind's package
// registers it (RegisterKind) from an init. Reads never decide who may see
// what: they read, for the person, what the access rule needs.
type Kind interface {
	Schema() Schema
	// Rows reads the named nodes, live or not, in no order.
	Rows(ctx context.Context, viewerUID string, uuids []string) ([]Row, error)
	// Children reads every live child of a live parent, at most limit, in no
	// order. ErrNotFound for no live parent.
	Children(ctx context.Context, viewerUID, parentUUID string, limit int) ([]Row, error)
	// Top reads the top level of a space as the person sees it, in no order;
	// "" is the kind's first space.
	Top(ctx context.Context, viewerUID, space string, limit int) ([]Row, error)
	// TopFor reads the top level the node with uuid sits in when it has no
	// parent, as the person sees it: where a move to the top puts it. With
	// leaving, the move takes it out of its space too, and it is the top level
	// it lands in without one.
	TopFor(ctx context.Context, viewerUID, uuid string, leaving bool, limit int) ([]Row, error)
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// RegisterKind makes a kind of tree known to business/Tree.
func RegisterKind(k Kind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds[k.Schema().Kind] = k
}

// KindFor is a registered kind of tree.
func KindFor(name string) (Kind, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	k, ok := kinds[name]
	return k, ok
}

// Ref is a node as the tree's structure knows it.
type Ref struct {
	UID        string
	UUID       string
	ParentUUID string // set by Subtree
	// SpaceKind and SpaceUUID are the space it is in, set by Subtree for a
	// kind with spaces ("" for none).
	SpaceKind, SpaceUUID string
}

// node is one level of a recursive read, decoded by the schema's names.
type node map[string]json.RawMessage

func (n node) str(key string) string {
	var s string
	_ = json.Unmarshal(n[key], &s)
	return s
}

// edge reads a uid edge, which the graph writes as an object for a single
// uid and as a list for a list (or under @recurse, either).
func (n node) edge(key string) []node {
	raw, ok := n[key]
	if !ok {
		return nil
	}
	var many []node
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one node
	if json.Unmarshal(raw, &one) == nil && one != nil {
		return []node{one}
	}
	return nil
}

// Chain is the node with id and the nodes above it, the node first, at most
// MaxDepth above it; truncated says the walk stopped with more above. A parent
// that isn't live ends the chain. ErrNotFound for no live node.
func Chain(ctx context.Context, s Schema, id string) (chain []Ref, truncated bool, err error) {
	if !s.valid() {
		return nil, false, ErrBadSchema
	}
	q := fmt.Sprintf(`query chain($id: string) {
		chain(func: eq(%[1]s, $id)) @filter(%[2]s) @recurse(depth: %[3]d, loop: false) {
			uid
			%[1]s
			%[4]s @filter(%[2]s)
		}
	}`, s.ID, s.Live, MaxDepth+2, s.Parent)
	var out struct {
		Chain []node `json:"chain"`
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": id}, &out); err != nil {
		return nil, false, err
	}
	if len(out.Chain) == 0 {
		return nil, false, ErrNotFound
	}
	for n := out.Chain[0]; n != nil; {
		if len(chain) == MaxDepth+1 {
			return chain, true, nil
		}
		chain = append(chain, Ref{UID: n.str("uid"), UUID: n.str(s.ID)})
		up := n.edge(s.Parent)
		if len(up) == 0 {
			break
		}
		n = up[0]
	}
	return chain, false, nil
}

// Subtree is every live node under the node with id (not the node itself), at
// most limit, parents before their children, with how many levels it goes
// down (0 for a node with no children), and for a kind with spaces the space
// each is in. ErrNotFound for no live node.
func Subtree(ctx context.Context, s Schema, id string, limit int) (under []Ref, height int, err error) {
	if !s.valid() {
		return nil, 0, ErrBadSchema
	}
	spaces := ""
	if s.SpaceKind != "" {
		spaces = s.SpaceKind + " " + s.SpaceUUID
	}
	q := fmt.Sprintf(`query sub($id: string) {
		root(func: eq(%[1]s, $id)) @filter(%[2]s) @recurse(depth: %[3]d, loop: false) {
			uid
			%[1]s %[5]s
			~%[4]s @filter(%[2]s)
		}
	}`, s.ID, s.Live, MaxDepth+2, s.Parent, spaces)
	var out struct {
		Root []node `json:"root"`
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": id}, &out); err != nil {
		return nil, 0, err
	}
	if len(out.Root) == 0 {
		return nil, 0, ErrNotFound
	}
	// Breadth first: a parent is always listed before its children.
	level := []node{out.Root[0]}
	for depth := 0; len(level) > 0; depth++ {
		var next []node
		for _, n := range level {
			for _, k := range n.edge("~" + s.Parent) {
				if len(under) >= limit {
					return under, depth + 1, nil
				}
				r := Ref{UID: k.str("uid"), UUID: k.str(s.ID), ParentUUID: n.str(s.ID)}
				if s.SpaceKind != "" {
					r.SpaceKind, r.SpaceUUID = k.str(s.SpaceKind), k.str(s.SpaceUUID)
				}
				under = append(under, r)
				next = append(next, k)
			}
		}
		if len(next) > 0 {
			height = depth + 1
		}
		level = next
	}
	return under, height, nil
}

// Siblings is every live child of the node with parentID, as its level is
// ordered (position.Order), at most limit: what a node made or moved under it
// is placed among. ErrNotFound for no live parent, and ErrLeaf for one that
// holds no others (Schema.Leaf), read in the same query.
func Siblings(ctx context.Context, s Schema, parentID string, limit int) ([]position.Sibling, error) {
	if !s.valid() {
		return nil, ErrBadSchema
	}
	leaf := ""
	if s.Leaf != "" {
		leaf = fmt.Sprintf("\n\t\tleaf(func: eq(%s, $id)) @filter(%s) { uid }", s.ID, s.Leaf)
	}
	q := fmt.Sprintf(`query kids($id: string) {
		parent(func: eq(%[1]s, $id)) @filter(%[2]s) {
			uid
			kids: ~%[3]s(first: %[4]d) @filter(%[2]s) {
				%[1]s
				%[5]s
				%[6]s
			}
		}%[7]s
	}`, s.ID, s.Live, s.Parent, limit, s.Position, s.Created, leaf)
	var out struct {
		Parent []struct {
			Kids []node `json:"kids"`
		} `json:"parent"`
		Leaf []node `json:"leaf"`
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": parentID}, &out); err != nil {
		return nil, err
	}
	if len(out.Parent) == 0 {
		return nil, ErrNotFound
	}
	if len(out.Leaf) > 0 {
		return nil, ErrLeaf
	}
	sibs := make([]position.Sibling, 0, len(out.Parent[0].Kids))
	for _, k := range out.Parent[0].Kids {
		var made time.Time
		_ = json.Unmarshal(k[s.Created], &made)
		sibs = append(sibs, position.Sibling{ID: k.str(s.ID), Position: k.str(s.Position), CreatedAt: made})
	}
	position.Order(sibs)
	return sibs, nil
}

// Move is one node going under a new parent ("" for the top), at a key.
type Move struct {
	UUID   string
	Parent string
	Key    string
	// Renumber gives siblings fresh keys in the same transaction (position.Place),
	// by id.
	Renumber map[string]string
	// Set writes more of the node's predicates with the move, the same value on
	// the node and on every node in Under (a doc's space, which its subtree
	// follows); an empty value removes the predicate.
	Set   map[string]string
	Under []string // uids
}

// Apply writes a move in one upsert that checks, in the transaction that
// writes, that the node and its new parent are live, that the parent isn't
// the node or under it and holds others (Schema.Leaf), and writes the tree's
// stamp. ErrNotFound, ErrCycle, ErrLeaf.
func Apply(ctx context.Context, s Schema, m Move, now time.Time) error {
	if !s.valid() {
		return ErrBadSchema
	}
	for k := range m.Set {
		if !predicateName.MatchString(k) {
			return ErrBadSchema
		}
	}
	for _, u := range m.Under {
		if !helpers.IsDgraphUID(u) {
			return ErrBadSchema
		}
	}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }

	var q strings.Builder
	fmt.Fprintf(&q, `query q($node: string, $parent: string) {
		node as var(func: eq(%[1]s, $node)) @filter(%[2]s)
		n(func: uid(node)) { uid }
		%[3]s`, s.ID, s.Live, stampQuery(s.Kind))
	cond := "@if(eq(len(node), 1))"
	if m.Parent != "" {
		fmt.Fprintf(&q, `
		target as var(func: eq(%[1]s, $parent)) @filter(%[2]s)
		var(func: uid(target)) @recurse(depth: %[3]d, loop: false) { above as %[4]s }
		loop as var(func: uid(node)) @filter(uid(target) OR uid(above))
		t(func: uid(target)) { uid }
		l(func: uid(loop)) { uid }`, s.ID, s.Live, MaxDepth+2, s.Parent)
		cond = "@if(eq(len(node), 1) AND eq(len(target), 1) AND eq(len(loop), 0))"
		if s.Leaf != "" {
			fmt.Fprintf(&q, `
		leafT as var(func: uid(target)) @filter(%s)
		lt(func: uid(leafT)) { uid }`, s.Leaf)
			cond = "@if(eq(len(node), 1) AND eq(len(target), 1) AND eq(len(loop), 0) AND eq(len(leafT), 0))"
		}
	}
	// Renumbered siblings by id: a sibling deleted meanwhile is left alone
	// (its variable is empty, and a mutation on an empty variable is skipped
	// by naming it only in its own conditional mutation).
	ids := make([]string, 0, len(m.Renumber))
	for id := range m.Renumber {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var muts []*api.Mutation
	for i, id := range ids {
		fmt.Fprintf(&q, "\n\t\tsib%d as var(func: eq(%s, %s))", i, s.ID, quote(id))
		muts = append(muts, &api.Mutation{
			SetNquads: []byte(fmt.Sprintf("uid(sib%d) <%s> %s .\n", i, s.Position, quote(m.Renumber[id]))),
			Cond:      fmt.Sprintf("%s AND eq(len(sib%d), 1))", strings.TrimSuffix(cond, ")"), i),
		})
	}
	q.WriteString("\n\t}")

	var set strings.Builder
	if m.Parent != "" {
		fmt.Fprintf(&set, "uid(node) <%s> uid(target) .\n", s.Parent)
	}
	fmt.Fprintf(&set, "uid(node) <%s> %s .\n", s.Position, quote(m.Key))
	keys := make([]string, 0, len(m.Set))
	for k := range m.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var del strings.Builder
	for _, k := range keys {
		if m.Set[k] == "" {
			fmt.Fprintf(&del, "uid(node) <%s> * .\n", k)
			for _, u := range m.Under {
				fmt.Fprintf(&del, "<%s> <%s> * .\n", u, k)
			}
			continue
		}
		fmt.Fprintf(&set, "uid(node) <%s> %s .\n", k, quote(m.Set[k]))
		for _, u := range m.Under {
			fmt.Fprintf(&set, "<%s> <%s> %s .\n", u, k, quote(m.Set[k]))
		}
	}
	writeStamp(&set, s.Kind, now)
	muts = append([]*api.Mutation{{SetNquads: []byte(set.String()), Cond: cond}}, muts...)
	if m.Parent == "" {
		fmt.Fprintf(&del, "uid(node) <%s> * .\n", s.Parent)
	}
	if del.Len() > 0 {
		muts = append(muts, &api.Mutation{DelNquads: []byte(del.String()), Cond: cond})
	}

	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     q.String(),
		Vars:      map[string]string{"$node": m.UUID, "$parent": m.Parent},
		Mutations: muts,
	})
	if err != nil {
		return err
	}
	var found struct {
		N  []struct{ UID string } `json:"n"`
		T  []struct{ UID string } `json:"t"`
		L  []struct{ UID string } `json:"l"`
		LT []struct{ UID string } `json:"lt"`
	}
	if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
		return err
	}
	switch {
	case len(found.N) != 1:
		return ErrNotFound
	case m.Parent != "" && len(found.T) != 1:
		return ErrNotFound
	case len(found.L) > 0:
		return ErrCycle
	case len(found.LT) > 0:
		return ErrLeaf
	}
	return nil
}

// stampQuery names the tree's stamp in an upsert's query, as "stamp".
func stampQuery(kind string) string {
	k, _ := json.Marshal(kind)
	return fmt.Sprintf("stamp as var(func: eq(tree_stamp_kind, %s))", k)
}

// writeStamp writes the tree's stamp: any change to a tree's shape writes it,
// so two at once are run one after the other by the graph. The first write
// makes the stamp's node (tree_stamp_kind is @upsert, so two first writes at
// once collide too).
func writeStamp(set *strings.Builder, kind string, now time.Time) {
	k, _ := json.Marshal(kind)
	at, _ := json.Marshal(now.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(set, "uid(stamp) <dgraph.type> \"TreeStamp\" .\nuid(stamp) <tree_stamp_kind> %s .\nuid(stamp) <tree_stamp_at> %s .\n", k, at)
}

// Stamped adds the tree's stamp to an upsert another package writes (a
// delete, a restore): its query gains the "stamp" variable and its mutation
// the stamp's write, so it is run one after any move it collides with.
func Stamped(kind string, query string, set *strings.Builder, now time.Time) string {
	writeStamp(set, kind, now)
	i := strings.LastIndex(query, "}")
	if i < 0 {
		return query
	}
	return query[:i] + "\t" + stampQuery(kind) + "\n" + query[i:]
}
