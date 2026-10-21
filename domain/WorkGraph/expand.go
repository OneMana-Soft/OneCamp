package workGraphDomain

// Expand is one hop out from several things at once, in one query (DG7):
// what the Ask AI context adds beside the semantic search's hits, so an
// answer can follow "this doc is linked from that task, in that project".
//
// Same allow-list, same rules as Neighbours: every hop live and capped, its
// targets named by their kind's uuid predicate, the roots found by their
// uuids (validated here, never a graph uid). No hub scan: a channel's and a
// person's posts are not followed from a seed. Each target carries its last
// change, so a caller can put the most recent first.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// ExpandMaxRoots bounds how many things one Expand reads from.
const ExpandMaxRoots = 12

// Near is a thing one hop from a root: the relationship from the root's side,
// and when the thing last changed (zero when the graph has no time for it).
type Near struct {
	Root    Ref
	Rel     string
	Pred    string
	Node    Ref
	Changed time.Time
}

// changedPred is a kind's last-change predicate (doc_updated_at, ...).
func changedPred(k Kind) string {
	return strings.TrimSuffix(specs[k].UUID, "uuid") + "updated_at"
}

// ExpandQuery is the read of what is one hop from roots, for kinds, at most
// perEdge targets of each edge from each root: one block per kind of root,
// the roots inlined by uuid (each checked as one). An empty query for no
// roots it can read.
func ExpandQuery(roots []Ref, kinds []Kind, perEdge int) (string, error) {
	if perEdge < 1 {
		return "", fmt.Errorf("work graph: perEdge %d", perEdge)
	}
	byKind := map[Kind][]string{}
	var order []Kind
	n := 0
	for _, r := range roots {
		if _, ok := specs[r.Kind]; !ok {
			return "", fmt.Errorf("work graph: no kind %q", r.Kind)
		}
		if !helpers.IsCanonicalUUID(r.UUID) {
			return "", fmt.Errorf("work graph: %q is not a uuid", r.UUID)
		}
		id := strings.ToLower(r.UUID)
		if containsString(byKind[r.Kind], id) {
			continue
		}
		if n == ExpandMaxRoots {
			break
		}
		if byKind[r.Kind] == nil {
			order = append(order, r.Kind)
		}
		byKind[r.Kind] = append(byKind[r.Kind], id)
		n++
	}
	if n == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("{\n")
	for _, k := range order {
		quoted := make([]string, 0, len(byKind[k]))
		for _, id := range byKind[k] {
			quoted = append(quoted, strconv.Quote(id))
		}
		fmt.Fprintf(&b, "\tr_%s(func: eq(%s, [%s])) @filter(%s) {\n\t\t%s\n", k, specs[k].UUID, strings.Join(quoted, ", "), liveOf([]Kind{k}), specs[k].UUID)
		for _, i := range edgesFrom([]Kind{k}, wants(kinds, nil), false) {
			e := Edges[i]
			var when []string
			for _, t := range e.To {
				when = append(when, changedPred(t))
			}
			fmt.Fprintf(&b, "\t\te_%d: %s @filter(%s) (first: %d) {\n\t\t\tuid %s %s\n\t\t}\n", i, e.Pred, liveOf(e.To), perEdge, uuidPreds(e.To), strings.Join(when, " "))
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}")
	return b.String(), nil
}

// Expand is every thing one hop from roots (at most ExpandMaxRoots of them),
// for kinds, perEdge of each edge from each root: in the allow-list's order
// per root, roots in the order given. A root that isn't there, or is
// deleted, adds nothing. One query.
func Expand(ctx context.Context, roots []Ref, kinds []Kind, perEdge int) ([]Near, error) {
	q, err := ExpandQuery(roots, kinds, perEdge)
	if err != nil || q == "" {
		return nil, err
	}
	raw, err := read(ctx, q, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph Expand: %+v", err)
		return nil, err
	}
	return parseExpand(raw, roots)
}

// parseExpand reads an ExpandQuery's answer, roots in the order given.
func parseExpand(raw map[string]json.RawMessage, roots []Ref) ([]Near, error) {
	got := map[Ref][]Near{}
	for key, block := range raw {
		if !strings.HasPrefix(key, "r_") {
			continue
		}
		k := Kind(key[2:])
		if _, ok := specs[k]; !ok {
			continue
		}
		var nodes []map[string]any
		if err := json.Unmarshal(orEmpty(block), &nodes); err != nil {
			return nil, fmt.Errorf("work graph: %s: %w", key, err)
		}
		for _, n := range nodes {
			rk, id := kindOf(n, []Kind{k})
			if rk == "" {
				continue
			}
			root := Ref{rk, id}
			got[root] = append(got[root], nearOf(root, n)...)
		}
	}
	var out []Near
	done := map[Ref]bool{}
	for _, r := range roots {
		r = Ref{r.Kind, strings.ToLower(r.UUID)}
		if done[r] {
			continue
		}
		done[r] = true
		out = append(out, got[r]...)
	}
	return out, nil
}

// nearOf is what one root node's edge blocks reach, in the allow-list's order.
func nearOf(root Ref, n map[string]any) []Near {
	var idx []int
	for key := range n {
		if !strings.HasPrefix(key, "e_") {
			continue
		}
		if i, err := strconv.Atoi(key[2:]); err == nil && i >= 0 && i < len(Edges) {
			idx = append(idx, i)
		}
	}
	sortInts(idx)
	var out []Near
	for _, i := range idx {
		e := Edges[i]
		for _, child := range asNodes(n["e_"+strconv.Itoa(i)]) {
			ck, id := kindOf(child, e.To)
			if ck == "" || (Ref{ck, id}) == root {
				continue
			}
			var changed time.Time
			if s, ok := child[changedPred(ck)].(string); ok {
				changed, _ = time.Parse(time.RFC3339Nano, s)
			}
			out = append(out, Near{Root: root, Rel: e.Rel, Pred: e.Pred, Node: Ref{ck, id}, Changed: changed})
		}
	}
	return out
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
