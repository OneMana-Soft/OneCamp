package workGraphDomain

// Thin nodes (DG12): the graph's nodes for things Postgres owns, a meeting
// among them (projectedTypes). Each is a projection that business/Refs keeps
// from its kind's projector, and nothing else writes it: its uuid, when it
// was deleted and last changed, and its own edges, which are the allow-list's
// forward edges from its kind (nodeEdges). A node can always be rebuilt from
// Postgres, and a write is only the difference between the node and its
// projection (NodeDiff).
//
// A node is found by its uuid inside the request that writes it, and made
// there when it isn't there yet. The uuid predicate is @upsert, so two
// writers making one node conflict and one tries again. Each added edge's
// target is found by its uuid in that request too, and each removed edge is
// named by the uid the node's own edge had, so no uid from outside the graph
// reaches a write.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// Projection is what Postgres says a thing's node should be.
type Projection struct {
	// Deleted is when it went (deleted or cancelled); zero while it's live.
	Deleted time.Time
	// Changed is when it last changed: its node's updated_at.
	Changed time.Time
	// Edges are its edges by predicate, each one of its kind's forward edges.
	Edges map[string][]Ref
}

// Node is a projected thing's node as the graph holds it.
type Node struct {
	UID     string
	UUID    string
	Deleted time.Time
	Changed time.Time
	// Have is its own edges by predicate, each target as the edge names it.
	Have map[string][]Target
}

// NodeFix is one node's change. A dropped node goes with its edges; any
// other is made or kept with its times, and its edges changed by predicate.
type NodeFix struct {
	Kind    Kind
	UUID    string
	Drop    bool
	Deleted time.Time
	Changed time.Time
	Edges   []EdgeFix
}

// EdgeFix is one predicate's change on a node: the edges to take off (by the
// target uids the node's own edges named) and the targets to add (by uuid).
type EdgeFix struct {
	Pred string
	Del  []string
	Add  []Ref
}

// nodeEdges is k's forward edges, the ones its node holds, in the
// allow-list's order.
func nodeEdges(k Kind) []Edge {
	var out []Edge
	for _, e := range Edges {
		if e.From == k && !strings.HasPrefix(e.Pred, "~") {
			out = append(out, e)
		}
	}
	return out
}

// NodeDiff is the change that makes node (nil: there is none) what p says
// (nil: there should be none), and whether it changes anything. A projection
// naming a predicate its kind has no forward edge for is refused: a
// projector writes only the allow-list. Pure.
func NodeDiff(k Kind, id string, node *Node, p *Projection) (NodeFix, bool, error) {
	f := NodeFix{Kind: k, UUID: strings.ToLower(id)}
	if p == nil {
		f.Drop = node != nil
		return f, f.Drop, nil
	}
	edges := nodeEdges(k)
	for pred := range p.Edges {
		if _, ok := EdgeFrom(k, pred); !ok || strings.HasPrefix(pred, "~") {
			return f, false, fmt.Errorf("work graph: a %s has no edge %q", k, pred)
		}
	}
	f.Deleted, f.Changed = p.Deleted.UTC(), p.Changed.UTC()
	changed := node == nil || !node.Deleted.Equal(p.Deleted) || !node.Changed.Equal(p.Changed)
	for _, e := range edges {
		var have []Target
		if node != nil {
			have = node.Have[e.Pred]
		}
		del, add := diffEdges(have, p.Edges[e.Pred])
		if len(del) > 0 || len(add) > 0 {
			f.Edges = append(f.Edges, EdgeFix{Pred: e.Pred, Del: del, Add: add})
			changed = true
		}
	}
	return f, changed, nil
}

// NodeRequest is the one upsert that applies fixes: each node found by its
// uuid in the query, as a variable n<i>, and made when it isn't there; its
// type and times set; its edges changed (writeEdges); a dropped one deleted
// with every predicate its type holds. Empty for no fixes.
func NodeRequest(fixes []NodeFix) (q, set, del string, err error) {
	var qb, sb, db strings.Builder
	for i, f := range fixes {
		typ, ok := projectedTypes[f.Kind]
		if !ok {
			return "", "", "", fmt.Errorf("work graph: %q isn't a projected kind", f.Kind)
		}
		if !helpers.IsCanonicalUUID(f.UUID) {
			return "", "", "", fmt.Errorf("work graph: a %s node for %q, which is no uuid", f.Kind, f.UUID)
		}
		s, id, v := specs[f.Kind], strings.ToLower(f.UUID), fmt.Sprintf("n%d", i)
		fmt.Fprintf(&qb, "\t%s as var(func: eq(%s, %q))\n", v, s.UUID, id)
		node := "uid(" + v + ")"
		if f.Drop {
			fmt.Fprintf(&db, "%s * * .\n", node)
			continue
		}
		fmt.Fprintf(&sb, "%s <%s> %q .\n", node, s.UUID, id)
		fmt.Fprintf(&sb, "%s <dgraph.type> %q .\n", node, typ)
		fmt.Fprintf(&sb, "%s <%s> %q .\n", node, s.Deleted, f.Deleted.UTC().Format(time.RFC3339Nano))
		fmt.Fprintf(&sb, "%s <%s> %q .\n", node, changedPred(f.Kind), f.Changed.UTC().Format(time.RFC3339Nano))
		for j, e := range f.Edges {
			edge, ok := EdgeFrom(f.Kind, e.Pred)
			if !ok || strings.HasPrefix(e.Pred, "~") {
				return "", "", "", fmt.Errorf("work graph: a %s has no edge %q", f.Kind, e.Pred)
			}
			if err := writeEdges(&qb, &sb, &db, node, e.Pred, edge.To, fmt.Sprintf("%d_%d", i, j), e.Del, e.Add); err != nil {
				return "", "", "", err
			}
		}
	}
	if qb.Len() > 0 {
		q = "query {\n" + qb.String() + "}"
	}
	return q, sb.String(), db.String(), nil
}

// WriteNodes applies fixes in one request; nothing is sent for none.
func WriteNodes(ctx context.Context, fixes []NodeFix) error {
	if len(fixes) == 0 {
		return nil
	}
	q, set, del, err := NodeRequest(fixes)
	if err != nil {
		return err
	}
	if err := mutate(ctx, q, dgexec.Mutation{SetNQuads: set, DelNQuads: del}); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph WriteNodes: %+v", err)
		return err
	}
	return nil
}

// nodeFields is what a node read takes of each of k's nodes: its names, its
// times and its own edges' targets.
func nodeFields(k Kind) string {
	s := specs[k]
	var b strings.Builder
	fmt.Fprintf(&b, "uid uuid: %s deleted: %s changed: %s", s.UUID, s.Deleted, changedPred(k))
	for _, e := range nodeEdges(k) {
		fmt.Fprintf(&b, " %s { uid %s }", e.Pred, uuidPreds(e.To))
	}
	return b.String()
}

// ReadNodes reads k's nodes by uuid, each with its own edges, by lower-case
// uuid: at most a page's worth, as the caller bounds ids. One query.
func ReadNodes(ctx context.Context, k Kind, ids []string) (map[string]*Node, error) {
	if !IsProjected(k) {
		return nil, fmt.Errorf("work graph: %q isn't a projected kind", k)
	}
	out := map[string]*Node{}
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return out, nil
	}
	raw, err := read(ctx, fmt.Sprintf("{\n\tn(func: eq(%s, [%s])) { %s }\n}", specs[k].UUID, list, nodeFields(k)), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WorkGraph ReadNodes %s: %+v", k, err)
		return nil, err
	}
	var nodes []map[string]any
	if err := json.Unmarshal(orEmpty(raw["n"]), &nodes); err != nil {
		return nil, fmt.Errorf("work graph: nodes: %w", err)
	}
	for _, n := range nodes {
		uid, _ := n["uid"].(string)
		id, _ := n["uuid"].(string)
		if !helpers.IsDgraphUID(uid) || id == "" {
			continue
		}
		node := &Node{UID: uid, UUID: strings.ToLower(id), Deleted: timeOf(n["deleted"]), Changed: timeOf(n["changed"]), Have: map[string][]Target{}}
		for _, e := range nodeEdges(k) {
			node.Have[e.Pred] = targetsOf(asNodes(n[e.Pred]), e.To)
		}
		out[node.UUID] = node
	}
	return out, nil
}

// timeOf is a dateTime as a read answers it; zero for none.
func timeOf(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
