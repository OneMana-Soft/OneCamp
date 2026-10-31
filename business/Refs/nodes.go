package refsBusiness

// Thin nodes (DG12): the work graph's nodes for the things Postgres owns (a
// meeting: domain/WorkGraph's projected kinds). A kind's package registers a
// Projector, the one batched read that says what each of its things' nodes
// should be (domain/WorkGraph Projection).
//
// HOW A NODE IS KEPT. A change to a thing reaches this package in one of two
// ways: the resource event its writer already announces (every event's kind
// that names a projected kind), or NodesChanged, from a writer that announces
// nothing (an import). Either writes an outbox event naming the ids, never
// their data, and releases and delivers it at once. The sink (reconcileNodes)
// projects the ids as they are when it runs, reads their nodes, and writes
// the difference in one upsert, so running it twice changes nothing the
// second time and an event for something changed since does what Postgres
// says now. An id the projector leaves out has no node, and loses the one it
// had.
//
// A change whose event couldn't be written is caught by the nightly
// reconciler (passNodes), which pages each kind's ids from Postgres and
// fixes the drift; `go-one-camp workgraphrefs --apply` rebuilds every node.
//
// WHO SEES A NODE. No one by its existence: each node the work graph reads
// is decided as the reader by its kind's EntityLink reader, and a path
// through anything hidden is dropped whole.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
)

// The nodes' outbox topic, its one sink and its event's kind.
const (
	nodesTopic       = "workgraph.nodes"
	nodesSink        = "graph.nodes"
	kindNodesChanged = "nodes.changed"
	// nodePage is how many ids one event names, and one nightly page reads:
	// one projector read, one graph read and at most one write.
	nodePage = 100
)

// Projector keeps the nodes of one kind Postgres owns.
type Projector struct {
	// Project is the projection of each of ids that should have a node, by
	// id as asked, in one batched read; an id it leaves out has none.
	Project func(ctx context.Context, ids []string) (map[string]domain.Projection, error)
	// Page is the nightly pass's keyset read: at most n ids after the
	// cursor after ("" from the start), and the cursor after them ("" once
	// there are no more).
	Page func(ctx context.Context, after string, n int) (ids []string, next string, err error)
}

var (
	projectorsMu sync.RWMutex
	projectors   = map[domain.Kind]Projector{}
)

// RegisterProjector makes p the keeper of k's nodes, from k's package's init.
// A kind that isn't projected, a projector missing a read, or a second one
// panics: each is a mistake in the code, not in the data.
func RegisterProjector(k domain.Kind, p Projector) {
	projectorsMu.Lock()
	defer projectorsMu.Unlock()
	if !domain.IsProjected(k) || p.Project == nil || p.Page == nil {
		panic(fmt.Sprintf("work graph: %q needs to be a projected kind with both reads", k))
	}
	if _, dup := projectors[k]; dup {
		panic(fmt.Sprintf("work graph: the projector for %q is registered twice", k))
	}
	projectors[k] = p
}

func projectorOf(k domain.Kind) (Projector, bool) {
	projectorsMu.RLock()
	defer projectorsMu.RUnlock()
	p, ok := projectors[k]
	return p, ok
}

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: nodesTopic, Name: nodesSink, Run: runNodesSink})
	// The writers of a projected kind announce their changes already: each
	// one keeps its node.
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name: "workgraph.nodes",
		Wants: func(e resourceEventBusiness.Event) bool {
			_, ok := projectorOf(domain.Kind(e.ResourceKind))
			return ok
		},
		Run: func(ctx context.Context, e resourceEventBusiness.Event) {
			NodesChanged(ctx, domain.Kind(e.ResourceKind), []string{e.ResourceUUID})
		},
	})
}

// NodesChanged tells the work graph that k's things with ids changed, once
// the change is written: events of at most nodePage ids each, written in one
// statement and released in one. It never fails the caller: an event that
// couldn't be written is the nightly reconciler's.
func NodesChanged(ctx context.Context, k domain.Kind, ids []string) {
	if _, ok := projectorOf(k); !ok {
		return
	}
	ids = canonical(ids)
	var es []outboxBusiness.Entry
	for from := 0; from < len(ids); from += nodePage {
		es = append(es, nodesEntry(k, ids[from:min(from+nodePage, len(ids))]))
	}
	if len(es) == 0 {
		return
	}
	if err := enqueueReady(ctx, es...); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Refs: the node events of %d %ss weren't written, so tonight's reconcile keeps them: %v", len(ids), k, err)
	}
}

// nodesEntry is the event naming ids of k. Its key is its first id, so the
// events of different things run side by side. One thing's own event and a
// batch that names it may also run side by side; that is harmless, since
// each run projects Postgres as it is when it runs, and of two runs making
// one node, the @upsert aborts one, which the outbox tries again. Pure.
func nodesEntry(k domain.Kind, ids []string) outboxBusiness.Entry {
	return outboxBusiness.Entry{
		Topic: nodesTopic, Kind: kindNodesChanged, Key: "nodes:" + string(k) + ":" + ids[0],
		Entities: map[string]string{"kind": string(k), "ids": strings.Join(ids, ",")},
		Sinks:    []string{nodesSink},
	}
}

// canonical is ids as uuids in lower case, each once, in their order; what
// isn't a uuid is dropped. Pure.
func canonical(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if helpers.IsCanonicalUUID(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// runNodesSink is the outbox's sink: the nodes the event names made what
// their projections say. An event of a kind with no projector is done.
func runNodesSink(ctx context.Context, ev outboxBusiness.Event) error {
	return reconcileNodes(ctx, domain.Kind(ev.Entities["kind"]), strings.Split(ev.Entities["ids"], ","), true, &Report{})
}

// Seams for tests: the graph's reads and writes of nodes.
var (
	readNodes  = domain.ReadNodes
	writeNodes = domain.WriteNodes
)

// reconcileNodes makes the nodes of ids (at most nodePage) what their
// projections say, counting the drift in rep, and writes the fixes when
// apply: one projector read, one graph read and at most one write.
func reconcileNodes(ctx context.Context, k domain.Kind, ids []string, apply bool, rep *Report) error {
	p, ok := projectorOf(k)
	ids = canonical(ids)
	if !ok || len(ids) == 0 {
		return nil
	}
	rep.Read += len(ids)
	want, err := p.Project(ctx, ids)
	if err != nil {
		return err
	}
	have, err := readNodes(ctx, k, ids)
	if err != nil {
		return err
	}
	var fixes []domain.NodeFix
	for _, id := range ids {
		var proj *domain.Projection
		if w, ok := want[id]; ok {
			proj = &w
		}
		f, changed, err := domain.NodeDiff(k, id, have[id], proj)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		rep.Drifted++
		for _, e := range f.Edges {
			rep.Added += len(e.Add)
			rep.Removed += len(e.Del)
		}
		fixes = append(fixes, f)
	}
	if !apply || len(fixes) == 0 {
		return nil
	}
	return writeNodes(ctx, fixes)
}

// passNodes is the nightly pass over k's things: pages of ids from its
// projector's keyset read, each reconciled, from the cursor, within bounds.
func passNodes(ctx context.Context, k domain.Kind, opts Options, over func() bool) (Report, error) {
	rep := Report{Kind: k, Applied: opts.Apply}
	p, ok := projectorOf(k)
	if !ok {
		return rep, nil
	}
	err := pass(ctx, k, opts, over, nodePage, &rep, func(after string, n int) (string, error) {
		ids, next, err := p.Page(ctx, after, n)
		if err != nil {
			return "", err
		}
		return next, reconcileNodes(ctx, k, ids, opts.Apply, &rep)
	})
	return rep, err
}

// projectedKinds is the projected kinds with a projector, in a fixed order:
// the ones a pass reads after the bodies' kinds.
func projectedKinds() []domain.Kind {
	var out []domain.Kind
	for _, k := range domain.ProjectedKinds() {
		if _, ok := projectorOf(k); ok {
			out = append(out, k)
		}
	}
	return out
}
