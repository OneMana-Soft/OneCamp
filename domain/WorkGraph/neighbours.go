package workGraphDomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// Ref names one thing: its kind and its uuid (lower case once read).
type Ref struct {
	Kind Kind
	UUID string
}

// Hop is one step of a path: along an edge (its Rel and Pred, from the
// previous node's side) to a node.
type Hop struct {
	Rel  string
	Pred string
	To   Ref
}

// Reached is a node and one path from the root to it (its last hop's To is
// the node). A node reached more than one way is reached once per way.
type Reached struct {
	Node Ref
	Path []Hop
}

// ErrNotFound is a root that isn't there, or is deleted.
var ErrNotFound = errors.New("work graph: not found")

// Caps on every hop: the first Cap1 targets of an edge from the root, and
// the first Cap2 of an edge from a node one hop out. A super-node (#general,
// a project's thousands of tasks, a person with every doc) costs a read at
// most its cap.
const (
	Cap1 = 20
	Cap2 = 4
)

// Mids are the kinds a two-hop read goes on from. A person, a channel and a
// team are hubs: everything near one of them would be "connected" to
// everything else near it, so a read stops at them.
var Mids = []Kind{KindDoc, KindTask, KindProject, KindBoard, KindMessage, KindChat}

// scanHas is what a hub's scan starts from: the posts that share a doc.
const scanHas = "post_doc"

// wants is whether an edge leads to a kind asked for (an optional edge only
// when its kind is asked for by name).
func wants(kinds []Kind, also []Kind) func(Edge) bool {
	return func(e Edge) bool {
		for _, k := range e.To {
			if hasKind(kinds, k) || (!e.Opt && hasKind(also, k)) {
				return true
			}
		}
		return false
	}
}

// NeighboursQuery is the read of what is up to depth (1 or 2) hops from a
// root of kind, for kinds: one query, the root by its uuid ($id).
func NeighboursQuery(kind Kind, depth int, kinds []Kind) (string, error) {
	spec, ok := specs[kind]
	if !ok {
		return "", fmt.Errorf("work graph: no kind %q", kind)
	}
	if depth != 1 && depth != 2 {
		return "", fmt.Errorf("work graph: depth %d (1 or 2)", depth)
	}
	var mids []Kind
	if depth == 2 {
		mids = Mids
	}
	var b strings.Builder
	b.WriteString("query n($id: string) {\n")
	fmt.Fprintf(&b, "\tR as var(func: eq(%s, $id)) @filter(%s)\n", spec.UUID, liveOf([]Kind{kind}))
	b.WriteString("\troot(func: uid(R)) {\n\t\tuid\n")
	var scans []int
	for _, i := range edgesFrom([]Kind{kind}, wants(kinds, mids), true) {
		e := Edges[i]
		if e.Scan {
			scans = append(scans, i)
			continue
		}
		writeEdge(&b, i, Cap1, "\t\t", childEdges(e, kinds, mids))
	}
	b.WriteString("\t}\n")
	for _, i := range scans {
		e := Edges[i]
		fwd := strings.TrimPrefix(e.Pred, "~")
		fmt.Fprintf(&b, "\tS%d as var(func: has(%s)) @filter(uid_in(%s, uid(R)) AND %s)\n", i, scanHas, fwd, liveOf(e.To))
		fmt.Fprintf(&b, "\ts_%d(func: uid(S%d), orderdesc: post_created_at, first: %d) {\n\t\tuid %s\n", i, i, Cap1, uuidPreds(e.To))
		for _, c := range childEdges(e, kinds, mids) {
			writeEdge(&b, c, Cap2, "\t\t", nil)
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}")
	return b.String(), nil
}

// childEdges is what a two-hop read follows from e's targets: nothing on a
// one-hop read, or from a hub.
func childEdges(e Edge, kinds, mids []Kind) []int {
	var from []Kind
	for _, k := range e.To {
		if hasKind(mids, k) {
			from = append(from, k)
		}
	}
	if len(from) == 0 {
		return nil
	}
	return edgesFrom(from, wants(kinds, nil), false)
}

// writeEdge writes edge i as a block e_<i>, its first cap targets, live, by
// their names, with children under it at Cap2.
func writeEdge(b *strings.Builder, i, cap int, indent string, children []int) {
	e := Edges[i]
	fmt.Fprintf(b, "%se_%d: %s @filter(%s) (first: %d) {\n%s\tuid %s\n", indent, i, e.Pred, liveOf(e.To), cap, indent, uuidPreds(e.To))
	for _, c := range children {
		writeEdge(b, c, Cap2, indent+"\t", nil)
	}
	fmt.Fprintf(b, "%s}\n", indent)
}

// read runs a query (seam for tests).
var read = func(ctx context.Context, q string, vars map[string]string) (map[string]json.RawMessage, error) {
	return dgexec.Read[map[string]json.RawMessage](ctx, q, vars)
}

// Neighbours is every node up to depth hops from root, for kinds, each with
// the path that reached it (more than one entry for a node reached more than
// one way), the root's own hops first. ErrNotFound for a root that isn't
// there or is deleted. One query.
func Neighbours(ctx context.Context, root Ref, depth int, kinds []Kind) ([]Reached, error) {
	if !helpers.IsCanonicalUUID(root.UUID) {
		return nil, ErrNotFound
	}
	q, err := NeighboursQuery(root.Kind, depth, kinds)
	if err != nil {
		return nil, err
	}
	raw, err := read(ctx, q, map[string]string{"$id": strings.ToLower(root.UUID)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph Neighbours %s %s: %+v", root.Kind, root.UUID, err)
		return nil, err
	}
	return parseNeighbours(raw, Ref{Kind: root.Kind, UUID: strings.ToLower(root.UUID)})
}

// parseNeighbours reads a NeighboursQuery's answer.
func parseNeighbours(raw map[string]json.RawMessage, root Ref) ([]Reached, error) {
	var roots []map[string]any
	if err := json.Unmarshal(orEmpty(raw["root"]), &roots); err != nil {
		return nil, fmt.Errorf("work graph: root: %w", err)
	}
	if len(roots) == 0 {
		return nil, ErrNotFound
	}
	var out []Reached
	walk(roots[0], root.Kind, nil, root, &out)
	for key, block := range raw {
		if !strings.HasPrefix(key, "s_") {
			continue
		}
		i, err := strconv.Atoi(key[2:])
		if err != nil || i < 0 || i >= len(Edges) {
			continue
		}
		var nodes []map[string]any
		if err := json.Unmarshal(orEmpty(block), &nodes); err != nil {
			return nil, fmt.Errorf("work graph: %s: %w", key, err)
		}
		e := Edges[i]
		for _, n := range nodes {
			k, id := kindOf(n, e.To)
			if k == "" {
				continue
			}
			path := []Hop{{Rel: e.Rel, Pred: e.Pred, To: Ref{k, id}}}
			out = append(out, Reached{Node: Ref{k, id}, Path: path})
			walk(n, k, path, root, &out)
		}
	}
	sortReached(out)
	return out, nil
}

// walk adds every node under n (of kind k, reached by path) to out, leaving
// out the root and any node already on its path.
func walk(n map[string]any, k Kind, path []Hop, root Ref, out *[]Reached) {
	for key, v := range n {
		if !strings.HasPrefix(key, "e_") {
			continue
		}
		i, err := strconv.Atoi(key[2:])
		if err != nil || i < 0 || i >= len(Edges) {
			continue
		}
		e := Edges[i]
		for _, child := range asNodes(v) {
			ck, id := kindOf(child, e.To)
			if ck == "" {
				continue
			}
			ref := Ref{ck, id}
			if ref == root || onPath(path, ref) {
				continue
			}
			from := k
			if from != e.From {
				// A node that may be one of several kinds reaches through the
				// edge its own kind has (the predicate is the same).
				if alt, ok := EdgeFrom(from, e.Pred); ok {
					e = alt
				}
			}
			p := append(append([]Hop(nil), path...), Hop{Rel: e.Rel, Pred: e.Pred, To: ref})
			*out = append(*out, Reached{Node: ref, Path: p})
			walk(child, ck, p, root, out)
		}
	}
}

func onPath(path []Hop, r Ref) bool {
	for _, h := range path {
		if h.To == r {
			return true
		}
	}
	return false
}

// asNodes is a block's nodes: a list, or one node for a single uid edge.
func asNodes(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{t}
	}
	return nil
}

func orEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("[]")
	}
	return b
}

// sortReached puts shorter paths first, then the allow-list's order, then by
// id, so an answer is the same every time (map order is not).
func sortReached(rs []Reached) {
	rank := func(r Reached) []int {
		out := make([]int, 0, len(r.Path))
		for _, h := range r.Path {
			out = append(out, edgeRank(h.Pred))
		}
		return out
	}
	less := func(a, b Reached) bool {
		if len(a.Path) != len(b.Path) {
			return len(a.Path) < len(b.Path)
		}
		ra, rb := rank(a), rank(b)
		for i := range ra {
			if ra[i] != rb[i] {
				return ra[i] < rb[i]
			}
		}
		for i := range a.Path {
			if a.Path[i].To != b.Path[i].To {
				return a.Path[i].To.Kind+Kind(a.Path[i].To.UUID) < b.Path[i].To.Kind+Kind(b.Path[i].To.UUID)
			}
		}
		return false
	}
	sort.SliceStable(rs, func(i, j int) bool { return less(rs[i], rs[j]) })
}

func edgeRank(pred string) int {
	for i, e := range Edges {
		if e.Pred == pred {
			return i
		}
	}
	return len(Edges)
}
