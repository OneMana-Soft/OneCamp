package refsBusiness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// meetings is a projector over made-up meetings, and the graph's nodes of
// them, with counts of each read and write.
type meetings struct {
	want          map[string]domain.Projection
	nodes         map[string]*domain.Node
	ids           []string // every id, in the order Page reads them
	projects      int
	reads, writes int
	fixes         []domain.NodeFix
	failWrites    int
}

// installMeetings registers m as the meetings' projector and the graph as
// m's nodes, for the test.
func installMeetings(t *testing.T) *meetings {
	t.Helper()
	m := &meetings{want: map[string]domain.Projection{}, nodes: map[string]*domain.Node{}}
	prevRead, prevWrite := readNodes, writeNodes
	projectorsMu.Lock()
	prevProjector, had := projectors[domain.KindEvent]
	delete(projectors, domain.KindEvent)
	projectorsMu.Unlock()
	t.Cleanup(func() {
		readNodes, writeNodes = prevRead, prevWrite
		projectorsMu.Lock()
		delete(projectors, domain.KindEvent)
		if had {
			projectors[domain.KindEvent] = prevProjector
		}
		projectorsMu.Unlock()
	})
	RegisterProjector(domain.KindEvent, Projector{
		Project: func(_ context.Context, ids []string) (map[string]domain.Projection, error) {
			m.projects++
			out := map[string]domain.Projection{}
			for _, id := range ids {
				if p, ok := m.want[id]; ok {
					out[id] = p
				}
			}
			return out, nil
		},
		Page: func(_ context.Context, after string, n int) ([]string, string, error) {
			start := 0
			if after != "" {
				start = slices.Index(m.ids, after) + 1
			}
			end := min(start+n, len(m.ids))
			page := m.ids[start:end]
			if end == len(m.ids) {
				return page, "", nil
			}
			return page, page[len(page)-1], nil
		},
	})
	readNodes = func(_ context.Context, _ domain.Kind, ids []string) (map[string]*domain.Node, error) {
		m.reads++
		out := map[string]*domain.Node{}
		for _, id := range ids {
			if n := m.nodes[id]; n != nil {
				out[id] = n
			}
		}
		return out, nil
	}
	writeNodes = func(_ context.Context, fixes []domain.NodeFix) error {
		m.writes++
		if m.failWrites > 0 {
			m.failWrites--
			return errors.New("graph unavailable")
		}
		m.fixes = append(m.fixes, fixes...)
		for _, f := range fixes {
			if f.Drop {
				delete(m.nodes, f.UUID)
				continue
			}
			m.nodes[f.UUID] = stored(f.UUID, m.want[f.UUID])
		}
		return nil
	}
	return m
}

// stored is p as the graph holds it once written.
func stored(id string, p domain.Projection) *domain.Node {
	n := &domain.Node{UID: "0x1", UUID: id, Deleted: p.Deleted, Changed: p.Changed, Have: map[string][]domain.Target{}}
	for pred, refs := range p.Edges {
		for i, r := range refs {
			n.Have[pred] = append(n.Have[pred], domain.Target{UID: fmt.Sprintf("0x%x", 100+i), Ref: r})
		}
	}
	return n
}

var when = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

func withPerson(person string) domain.Projection {
	return domain.Projection{Changed: when, Edges: map[string][]domain.Ref{
		"cal_event_organiser": {{Kind: domain.KindPerson, UUID: self}},
		"cal_event_with":      {{Kind: domain.KindPerson, UUID: person}},
	}}
}

// TestNodesChangedWritesAnEventPerPageOfIDs: a change to many things is one
// outbox write of events naming at most a page of ids each, the ids cleaned
// (a uuid in lower case, once); nothing is written for a kind with no
// projector or for no ids.
func TestNodesChangedWritesAnEventPerPageOfIDs(t *testing.T) {
	f := install(t)
	installMeetings(t)
	var ids []string
	for i := 0; i < 2*nodePage+50; i++ {
		ids = append(ids, uuid.NewString())
	}
	messy := append([]string{strings.ToUpper(ids[0]), "not a uuid", ""}, ids...)
	NodesChanged(context.Background(), domain.KindEvent, messy)
	if f.enqueues != 1 || len(f.queued) != 3 {
		t.Fatalf("%d outbox writes of %d events, want one of three", f.enqueues, len(f.queued))
	}
	first := f.queued[0]
	if first.Topic != nodesTopic || first.Kind != kindNodesChanged || first.Key != "nodes:event:"+ids[0] ||
		first.Entities["kind"] != "event" || len(strings.Split(first.Entities["ids"], ",")) != nodePage || !slices.Equal(first.Sinks, []string{nodesSink}) {
		t.Errorf("first event %+v", first)
	}
	if got := strings.Split(f.queued[2].Entities["ids"], ","); len(got) != 50 || got[49] != ids[len(ids)-1] {
		t.Errorf("the last event names %d ids", len(got))
	}
	NodesChanged(context.Background(), domain.KindTask, ids)
	NodesChanged(context.Background(), domain.KindEvent, []string{"nope"})
	if f.enqueues != 1 {
		t.Errorf("a kind with no projector or no ids wrote %d times", f.enqueues-1)
	}
}

// TestTheSinkMakesNodesWhatTheirProjectionsSay: an event makes the node of
// each id with a projection and drops the node of one without; delivered
// again, it reads and changes nothing; a failed write is tried again; an
// event of a kind with no projector is done.
func TestTheSinkMakesNodesWhatTheirProjectionsSay(t *testing.T) {
	m := installMeetings(t)
	a, b, gone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	m.want[a], m.want[b] = withPerson(docA), withPerson(docB)
	m.nodes[gone] = stored(gone, withPerson(docA))
	ev := outboxBusiness.Event{Topic: nodesTopic, Entities: map[string]string{"kind": "event", "ids": strings.Join([]string{a, b, gone}, ",")}}
	m.failWrites = 1
	if err := runNodesSink(context.Background(), ev); err == nil {
		t.Fatal("a failed write was answered done")
	}
	if err := runNodesSink(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if m.projects != 2 || m.reads != 2 || m.writes != 2 || len(m.fixes) != 3 {
		t.Fatalf("%d projections, %d reads, %d writes, %d fixes", m.projects, m.reads, m.writes, len(m.fixes))
	}
	if m.nodes[a] == nil || m.nodes[b] == nil || m.nodes[gone] != nil {
		t.Errorf("nodes after the event: %v", m.nodes)
	}
	if err := runNodesSink(context.Background(), ev); err != nil || m.writes != 2 {
		t.Errorf("delivered again it wrote (%d writes, %v)", m.writes, err)
	}
	if err := runNodesSink(context.Background(), outboxBusiness.Event{Entities: map[string]string{"kind": "task", "ids": a}}); err != nil || m.projects != 3 {
		t.Errorf("an event of a kind with no projector: %v, %d projections", err, m.projects)
	}
}

// TestAProjectedKindsResourceEventKeepsItsNode: a writer's resource event
// about a projected kind's thing is that thing's node event; any other
// resource event is not the graph's.
func TestAProjectedKindsResourceEventKeepsItsNode(t *testing.T) {
	f := install(t)
	installMeetings(t)
	id := uuid.NewString()
	resourceEventBusiness.Bus.Emit(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarEventUpdated, ResourceKind: "event", ResourceUUID: id})
	resourceEventBusiness.Bus.Emit(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: id})
	helpers.SettleBackground(10 * time.Second)
	if len(f.queued) != 1 || f.queued[0].Entities["ids"] != id || f.queued[0].Entities["kind"] != "event" {
		t.Errorf("events queued: %+v", f.queued)
	}
}

// TestTheNightlyPassKeepsNodesFromItsCursor: a dry run counts the drift and
// writes nothing; a bounded pass stops at its bound with the cursor where it
// stopped, and the next goes on from there and wraps at the end, each page
// one projection, one read and one write.
func TestTheNightlyPassKeepsNodesFromItsCursor(t *testing.T) {
	m := installMeetings(t)
	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		m.ids = append(m.ids, id)
		if i%10 == 0 {
			m.want[id] = withPerson(docA)
		}
	}
	ctx := context.Background()
	reps, err := ReconcileAll(ctx, Options{Kinds: []domain.Kind{domain.KindEvent}})
	if err != nil || len(reps) != 1 {
		t.Fatal(err, reps)
	}
	if r := reps[0]; r.Read != 250 || r.Drifted != 25 || r.Added != 50 || !r.Wrapped || m.writes != 0 {
		t.Errorf("dry run %+v, %d writes", r, m.writes)
	}
	cur := memCursor{}
	reps, err = ReconcileAll(ctx, Options{Kinds: []domain.Kind{domain.KindEvent}, Apply: true, PerKind: 150, Cursor: cur})
	if err != nil || reps[0].Read != 150 || reps[0].Drifted != 15 || reps[0].Wrapped || cur[domain.KindEvent] != m.ids[149] {
		t.Fatalf("first bounded pass %+v cursor %q (%v)", reps[0], cur[domain.KindEvent], err)
	}
	reps, _ = ReconcileAll(ctx, Options{Kinds: []domain.Kind{domain.KindEvent}, Apply: true, PerKind: 150, Cursor: cur})
	if reps[0].Read != 100 || reps[0].Drifted != 10 || !reps[0].Wrapped || cur[domain.KindEvent] != "" {
		t.Errorf("second pass %+v cursor %q", reps[0], cur[domain.KindEvent])
	}
	if len(m.nodes) != 25 || m.writes != 3 {
		t.Errorf("%d nodes after %d writes, want 25 after 3", len(m.nodes), m.writes)
	}
}

// TestTheNightlyPassReadsBodiesThenNodes: with no kinds named, a pass reads
// every body kind and then each projected kind with a projector.
func TestTheNightlyPassReadsBodiesThenNodes(t *testing.T) {
	f := install(t)
	installMeetings(t)
	pageFakes(t, f, 0, func(int) string { return "" })
	reps, err := ReconcileAll(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []domain.Kind
	for _, r := range reps {
		kinds = append(kinds, r.Kind)
	}
	if want := append(slices.Clone(passKinds), domain.KindEvent); !slices.Equal(kinds, want) {
		t.Errorf("a pass read %v, want %v", kinds, want)
	}
}

// TestAProjectorIsRegisteredOnceForAProjectedKind: a kind the graph owns, a
// projector missing a read and a second projector are mistakes in the code.
func TestAProjectorIsRegisteredOnceForAProjectedKind(t *testing.T) {
	installMeetings(t)
	ok := Projector{
		Project: func(context.Context, []string) (map[string]domain.Projection, error) { return nil, nil },
		Page:    func(context.Context, string, int) ([]string, string, error) { return nil, "", nil },
	}
	for name, register := range map[string]func(){
		"a kind the graph owns": func() { RegisterProjector(domain.KindDoc, ok) },
		"no page read":          func() { RegisterProjector(domain.KindEvent, Projector{Project: ok.Project}) },
		"a second projector":    func() { RegisterProjector(domain.KindEvent, ok) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was registered", name)
				}
			}()
			register()
		}()
	}
}
