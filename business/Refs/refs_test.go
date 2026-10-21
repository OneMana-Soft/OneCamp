package refsBusiness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/google/uuid"
)

const (
	docA  = "11111111-1111-4111-8111-111111111111"
	docB  = "22222222-2222-4222-8222-222222222222"
	taskA = "33333333-3333-4333-8333-333333333333"
	projA = "44444444-4444-4444-8444-444444444444"
	self  = "55555555-5555-4555-8555-555555555555"
)

func chip(kind, id string) string {
	return `<span data-type="entityMention" data-id="` + kind + `:` + id + `"></span>`
}

// fakes swaps the graph and doc_task_links for counters.
type fakes struct {
	sources    map[string]*domain.Source
	tasks      map[uuid.UUID][]uuid.UUID
	reads      int
	taskReads  int
	writes     int
	written    []domain.Fix
	failWrites int
	queued     []outboxBusiness.Entry
}

func install(t *testing.T) *fakes {
	f := &fakes{sources: map[string]*domain.Source{}, tasks: map[uuid.UUID][]uuid.UUID{}}
	pr, pw, pt, pe := readSource, writeFixes, tasksForDocs, enqueueReady
	t.Cleanup(func() { readSource, writeFixes, tasksForDocs, enqueueReady = pr, pw, pt, pe })
	readSource = func(_ context.Context, k domain.Kind, id string) (*domain.Source, error) {
		f.reads++
		s := f.sources[id]
		if s == nil {
			return nil, nil
		}
		c := *s
		c.Kind = k
		return &c, nil
	}
	writeFixes = func(_ context.Context, fixes []domain.Fix) error {
		f.writes++
		if f.failWrites > 0 {
			f.failWrites--
			return errors.New("graph unavailable")
		}
		f.written = append(f.written, fixes...)
		return nil
	}
	tasksForDocs = func(_ context.Context, docs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
		f.taskReads++
		out := map[uuid.UUID][]uuid.UUID{}
		for _, d := range docs {
			out[d] = f.tasks[d]
		}
		return out, nil
	}
	enqueueReady = func(_ context.Context, e outboxBusiness.Entry) error {
		f.queued = append(f.queued, e)
		return nil
	}
	return f
}

// TestWhatEachSourceLinksTo: a message's every link, written out in text too;
// a doc's tasks and projects by chip or link (its docs are doc_mentions), and
// the tasks it holds to-dos for; never itself.
func TestWhatEachSourceLinksTo(t *testing.T) {
	body := chip("doc", docA) + chip("task", taskA) + " https://x/app/project/" + projA + " " + chip("doc", self)
	msg := Want(domain.Source{Kind: Message, UUID: self, Body: body}, nil)
	want := []domain.Ref{{Kind: domain.KindDoc, UUID: docA}, {Kind: domain.KindTask, UUID: taskA}, {Kind: domain.KindProject, UUID: projA}}
	if !reflect.DeepEqual(msg, want) {
		t.Errorf("message links %v, want %v", msg, want)
	}
	doc := Want(domain.Source{Kind: Doc, UUID: self, Body: body}, []string{taskA, strings.ToUpper(docB[:8]) + docB[8:]})
	want = []domain.Ref{{Kind: domain.KindTask, UUID: taskA}, {Kind: domain.KindTask, UUID: docB}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("doc links %v, want %v", doc, want)
	}
}

// TestANewBodyWithNoLinksWritesNoEvent: the common message costs nothing; an
// edit always writes one (it may have taken the last link out).
func TestANewBodyWithNoLinksWritesNoEvent(t *testing.T) {
	f := install(t)
	ctx := context.Background()
	Saved(ctx, Message, self, "<p>lunch?</p>", true)
	if len(f.queued) != 0 {
		t.Fatalf("a new body with no links queued %d events", len(f.queued))
	}
	Saved(ctx, Message, self, "<p>lunch?</p>", false)
	Saved(ctx, Chat, docB, chip("doc", docA), true)
	Saved(ctx, domain.KindTask, docB, chip("doc", docA), true) // not a source
	Saved(ctx, Message, "not-a-uuid", chip("doc", docA), true)
	if len(f.queued) != 2 {
		t.Fatalf("queued %d events, want 2: %+v", len(f.queued), f.queued)
	}
	e := f.queued[1]
	if e.Topic != Topic || e.Key != "refs:chat:"+docB || e.Entities["kind"] != "chat" || e.Entities["uuid"] != docB || len(e.Entities) != 2 {
		t.Errorf("event %+v: it names the source by kind and uuid, never its body", e)
	}
}

// TestASaveCostsTheSameRoundTripsHoweverManyLinks: one graph read and one
// write for a body with one link or a hundred; one read and no write when
// nothing changed; a doc adds one Postgres read.
func TestASaveCostsTheSameRoundTripsHoweverManyLinks(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{1, 100} {
		f := install(t)
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(chip("doc", uuid.NewString()))
		}
		f.sources[self] = &domain.Source{UID: "0x1", UUID: self, Body: b.String(), Have: []domain.Target{{UID: "0x9", Ref: domain.Ref{Kind: domain.KindDoc, UUID: docB}}}}
		fix, err := Reconcile(ctx, Message, self)
		if err != nil || f.reads != 1 || f.writes != 1 || f.taskReads != 0 || len(fix.Add) != n || len(fix.Del) != 1 {
			t.Errorf("%d links: %d reads, %d writes, %d task reads, fix %d+/%d- (%v)", n, f.reads, f.writes, f.taskReads, len(fix.Add), len(fix.Del), err)
		}
	}
	f := install(t)
	f.sources[self] = &domain.Source{UID: "0x1", UUID: self, Body: chip("task", taskA), Have: []domain.Target{{UID: "0x9", Ref: domain.Ref{Kind: domain.KindTask, UUID: taskA}}}}
	if _, err := Reconcile(ctx, Doc, self); err != nil || f.reads != 1 || f.writes != 0 || f.taskReads != 1 {
		t.Errorf("an unchanged doc: %d reads, %d writes, %d task reads (%v)", f.reads, f.writes, f.taskReads, err)
	}
	if fix, err := Reconcile(ctx, Message, docA); err != nil || !fix.Empty() || f.writes != 0 {
		t.Errorf("a source that's gone wrote %+v (%v)", fix, err)
	}
}

// TestASinkThatFailsIsTriedAgain: the sink answers the outbox an error when
// the write fails, so the outbox tries it again; the second try fixes it,
// and a third changes nothing.
func TestASinkThatFailsIsTriedAgain(t *testing.T) {
	f := install(t)
	f.failWrites = 1
	f.sources[self] = &domain.Source{UID: "0x1", UUID: self, Body: chip("doc", docA)}
	ev := outboxBusiness.Event{Topic: Topic, Entities: map[string]string{"kind": "message", "uuid": self}}
	if err := runSink(context.Background(), ev); err == nil {
		t.Fatal("a failed write was answered done")
	}
	if err := runSink(context.Background(), ev); err != nil || len(f.written) != 1 {
		t.Fatalf("the retry: %v, %d written", err, len(f.written))
	}
	f.sources[self].Have = []domain.Target{{UID: "0x7", Ref: domain.Ref{Kind: domain.KindDoc, UUID: docA}}}
	if err := runSink(context.Background(), ev); err != nil || f.writes != 2 {
		t.Fatalf("a third run wrote again (%d writes, %v)", f.writes, err)
	}
	if err := runSink(context.Background(), outboxBusiness.Event{Entities: map[string]string{"kind": "task", "uuid": self}}); err != nil {
		t.Errorf("an event naming no source: %v", err)
	}
}

// pageFakes serves pages of n sources of one kind.
func pageFakes(t *testing.T, f *fakes, total int, linked func(i int) string) {
	pr, pe := readPage, existing
	t.Cleanup(func() { readPage, existing = pr, pe })
	all := make([]domain.Source, total)
	for i := range all {
		all[i] = domain.Source{Kind: Message, UID: fmt.Sprintf("0x%x", i+1), UUID: uuid.NewString(), Body: linked(i)}
	}
	readPage = func(_ context.Context, k domain.Kind, after string, n int) ([]domain.Source, error) {
		f.reads++
		start := 0
		if after != "" {
			fmt.Sscanf(after, "0x%x", &start)
		}
		end := min(start+n, len(all))
		if start >= end {
			return nil, nil
		}
		return all[start:end], nil
	}
	existing = func(_ context.Context, refs []domain.Ref) (map[domain.Ref]bool, error) {
		out := map[domain.Ref]bool{}
		for _, r := range refs {
			out[r] = r.UUID != docB // docB names nothing
		}
		return out, nil
	}
}

type memCursor map[domain.Kind]string

func (c memCursor) Load(_ context.Context, k domain.Kind) string        { return c[k] }
func (c memCursor) Save(_ context.Context, k domain.Kind, after string) { c[k] = after }

// TestTheReconcilerReportsAndFixesDriftWithinItsBounds: a dry run reports
// and writes nothing; a link to nothing is no drift; the bound stops a pass
// and the cursor goes on from there the next time, wrapping at the end.
func TestTheReconcilerReportsAndFixesDriftWithinItsBounds(t *testing.T) {
	f := install(t)
	pageFakes(t, f, 1200, func(i int) string {
		switch {
		case i%100 == 0:
			return chip("doc", docA)
		case i%100 == 1:
			return chip("doc", docB)
		}
		return "<p>nothing</p>"
	})
	ctx := context.Background()
	reps, err := ReconcileAll(ctx, Options{Kinds: []domain.Kind{Message}})
	if err != nil || len(reps) != 1 {
		t.Fatal(err, reps)
	}
	if r := reps[0]; r.Read != 1200 || r.Drifted != 12 || r.Added != 12 || r.Removed != 0 || !r.Wrapped || f.writes != 0 {
		t.Errorf("dry run %+v, %d writes", r, f.writes)
	}
	cur := memCursor{}
	reps, err = ReconcileAll(ctx, Options{Kinds: []domain.Kind{Message}, Apply: true, PerKind: 700, Cursor: cur})
	if err != nil || reps[0].Read != 700 || reps[0].Drifted != 7 || reps[0].Wrapped || cur[Message] != "0x2bc" {
		t.Fatalf("first bounded pass %+v cursor %q (%v)", reps[0], cur[Message], err)
	}
	reps, _ = ReconcileAll(ctx, Options{Kinds: []domain.Kind{Message}, Apply: true, PerKind: 700, Cursor: cur})
	if reps[0].Read != 500 || reps[0].Drifted != 5 || !reps[0].Wrapped || cur[Message] != "" {
		t.Errorf("second pass %+v cursor %q", reps[0], cur[Message])
	}
	if len(f.written) != 12 {
		t.Errorf("%d fixes written, want 12", len(f.written))
	}
}
