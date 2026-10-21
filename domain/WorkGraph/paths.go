package workGraphDomain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// Path bounds: at most MaxPaths shortest paths, of at most MaxDepth hops,
// with the first PathCap targets of each edge from each node on the way (a
// project's thousands of tasks cost a search 25 of them).
const (
	MaxPaths = 3
	MaxDepth = 4
	PathCap  = 25
)

// Path is one way from a thing to another: its start and its hops.
type Path struct {
	From Ref
	Hops []Hop
}

// pathPreds is every predicate a shortest path may walk, each once, with
// the kinds it may lead to: the allow-list without its scans (a hub's posts)
// and its optional edges (comments and memory only lead back).
func pathPreds() ([]string, map[string][]Kind) {
	var order []string
	to := map[string][]Kind{}
	for _, e := range Edges {
		if e.Scan || e.Opt || e.From == KindComment || e.From == KindMemory {
			continue
		}
		if _, ok := to[e.Pred]; !ok {
			order = append(order, e.Pred)
		}
		for _, k := range e.To {
			if !hasKind(to[e.Pred], k) {
				to[e.Pred] = append(to[e.Pred], k)
			}
		}
	}
	return order, to
}

// PathsQuery is the read of the shortest paths from a thing of kind from
// ($a) to one of kind to ($b): one shortest block (Dgraph allows one a
// query), and the names of every node on the paths.
func PathsQuery(from, to Kind) (string, error) {
	fs, ok := specs[from]
	if !ok {
		return "", fmt.Errorf("work graph: no kind %q", from)
	}
	ts, ok := specs[to]
	if !ok {
		return "", fmt.Errorf("work graph: no kind %q", to)
	}
	var b strings.Builder
	b.WriteString("query p($a: string, $b: string) {\n")
	fmt.Fprintf(&b, "\tA as var(func: eq(%s, $a)) @filter(%s)\n", fs.UUID, liveOf([]Kind{from}))
	fmt.Fprintf(&b, "\tB as var(func: eq(%s, $b)) @filter(%s)\n", ts.UUID, liveOf([]Kind{to}))
	fmt.Fprintf(&b, "\tP as shortest(from: uid(A), to: uid(B), numpaths: %d, depth: %d) {\n", MaxPaths, MaxDepth)
	order, kinds := pathPreds()
	for _, p := range order {
		fmt.Fprintf(&b, "\t\t%s @filter(%s) (first: %d)\n", p, liveOf(kinds[p]), PathCap)
	}
	b.WriteString("\t}\n")
	fmt.Fprintf(&b, "\tnodes(func: uid(P)) { uid %s }\n", allUUIDPreds())
	b.WriteString("}")
	return b.String(), nil
}

// orient is whether a search from a to b should run from b instead: the
// graph walks into a channel (post_channel) and a person's posts (post_by)
// but not out of them, so a channel, or a person going anywhere but a
// channel, is better reached than started from.
func orient(a, b Kind) bool {
	switch {
	case a == KindChannel:
		return b != KindChannel
	case a == KindPerson:
		return b != KindChannel && b != KindPerson
	}
	return false
}

// Paths is at most MaxPaths shortest paths from a to b of at most MaxDepth
// hops, shortest first; none when a and b are the same thing, or either
// isn't there. One query.
func Paths(ctx context.Context, a, b Ref) ([]Path, error) {
	if !helpers.IsCanonicalUUID(a.UUID) || !helpers.IsCanonicalUUID(b.UUID) {
		return nil, ErrNotFound
	}
	a.UUID, b.UUID = strings.ToLower(a.UUID), strings.ToLower(b.UUID)
	if a == b {
		return nil, nil
	}
	swap := orient(a.Kind, b.Kind)
	from, to := a, b
	if swap {
		from, to = b, a
	}
	q, err := PathsQuery(from.Kind, to.Kind)
	if err != nil {
		return nil, err
	}
	raw, err := read(ctx, q, map[string]string{"$a": from.UUID, "$b": to.UUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph Paths %s %s -> %s %s: %+v", a.Kind, a.UUID, b.Kind, b.UUID, err)
		return nil, err
	}
	paths, err := parsePaths(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Path, 0, len(paths))
	for _, p := range paths {
		if swap {
			var ok bool
			if p, ok = reversePath(p); !ok {
				continue
			}
		}
		if p.From == a && len(p.Hops) > 0 && p.Hops[len(p.Hops)-1].To == b {
			out = append(out, p)
		}
	}
	return out, nil
}

// parsePaths reads a PathsQuery's answer: each path's nodes by name, its
// hops by the allow-list. A path with a node of no known kind, or a hop the
// allow-list doesn't have, is left out.
func parsePaths(raw map[string]json.RawMessage) ([]Path, error) {
	var nodes []map[string]any
	if err := json.Unmarshal(orEmpty(raw["nodes"]), &nodes); err != nil {
		return nil, fmt.Errorf("work graph: nodes: %w", err)
	}
	all := Kinds()
	byUID := map[string]Ref{}
	for _, n := range nodes {
		uid, _ := n["uid"].(string)
		if k, id := kindOf(n, all); k != "" && uid != "" {
			byUID[uid] = Ref{k, id}
		}
	}
	var trails []map[string]any
	if err := json.Unmarshal(orEmpty(raw["_path_"]), &trails); err != nil {
		return nil, fmt.Errorf("work graph: _path_: %w", err)
	}
	var out []Path
next:
	for _, t := range trails {
		uid, _ := t["uid"].(string)
		start, ok := byUID[uid]
		if !ok {
			continue
		}
		p := Path{From: start}
		at, cur := start, t
		for {
			pred, child := stepOf(cur)
			if pred == "" {
				break
			}
			cuid, _ := child["uid"].(string)
			ref, ok := byUID[cuid]
			if !ok {
				continue next
			}
			e, ok := EdgeFrom(at.Kind, pred)
			if !ok || !hasKind(e.To, ref.Kind) {
				continue next
			}
			p.Hops = append(p.Hops, Hop{Rel: e.Rel, Pred: pred, To: ref})
			at, cur = ref, child
			if len(p.Hops) > MaxDepth {
				continue next
			}
		}
		if len(p.Hops) > 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

// stepOf is a path level's one edge and the node it leads to.
func stepOf(level map[string]any) (string, map[string]any) {
	for k, v := range level {
		if k == "uid" || strings.HasPrefix(k, "_") {
			continue
		}
		if ns := asNodes(v); len(ns) == 1 {
			return k, ns[0]
		}
	}
	return "", nil
}

// reversePath is p walked from its end: each hop's predicate inverted, and
// named by the allow-list from its new side. False when the allow-list has
// no such edge (the path then isn't one the panel could word).
func reversePath(p Path) (Path, bool) {
	refs := []Ref{p.From}
	for _, h := range p.Hops {
		refs = append(refs, h.To)
	}
	out := Path{From: refs[len(refs)-1]}
	for i := len(p.Hops) - 1; i >= 0; i-- {
		from, to := refs[i+1], refs[i]
		pred := Inverse(p.Hops[i].Pred)
		e, ok := EdgeFrom(from.Kind, pred)
		if !ok {
			return Path{}, false
		}
		out.Hops = append(out.Hops, Hop{Rel: e.Rel, Pred: pred, To: to})
	}
	return out, true
}
