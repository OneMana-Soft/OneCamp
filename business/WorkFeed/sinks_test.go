package business

import (
	"context"
	"errors"
	"reflect"
	"testing"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
)

// freshSinks gives a test sinks of its own, in the Outbox's registry and the
// log's, and puts the server's back after.
func freshSinks(t *testing.T) {
	t.Helper()
	savedOutbox := outboxBusiness.Sinks
	sinksMu.Lock()
	savedKinds, savedHanded := sinksOf, handedTo
	sinksOf, handedTo = map[string][]string{}, map[string]outboxBusiness.Sink{}
	sinksMu.Unlock()
	outboxBusiness.Sinks = &outboxBusiness.Registry{}
	t.Cleanup(func() {
		outboxBusiness.Sinks = savedOutbox
		sinksMu.Lock()
		sinksOf, handedTo = savedKinds, savedHanded
		sinksMu.Unlock()
	})
}

func nothing(context.Context, string, Entry) error { return nil }

// An entry a sink wants is one event for it, naming the entry by its number,
// keyed by its task so one task's changes reach the sink in the order they
// were made; an entry no sink wants is none.
func TestAnEntryIsAnEventForTheSinksThatWantIt(t *testing.T) {
	freshSinks(t)
	RegisterSink(Sink{Name: "hooks", Kinds: []string{KindUpdated}, Run: nothing})
	RegisterSink(Sink{Name: "history", Kinds: []string{KindUpdated, KindDeleted}, Run: nothing})
	got := eventsFor(context.Background(), []Change{change("t1", KindUpdated), change("t2", KindCreated), change("t3", KindDeleted)}, []int64{41, 42, 43})
	want := []outboxBusiness.Entry{
		{Topic: Topic, Kind: KindUpdated, Key: "task:t1", Actor: "u1", Sinks: []string{"hooks", "history"},
			Entities: map[string]string{"sequence": "41", "task": "t1", "project": "p1"}},
		{Topic: Topic, Kind: KindDeleted, Key: "task:t3", Actor: "u1", Sinks: []string{"history"},
			Entities: map[string]string{"sequence": "43", "task": "t3", "project": "p1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events %+v\nwant %+v", got, want)
	}
}

// A bulk import's entries reach no sink: nobody is sent history
// (DispatchEvent). A workflow's or an agent's write carries its marks across,
// so a listener it reaches knows it, and a workflow never wakes itself.
func TestAnEventCarriesTheWritersMarks(t *testing.T) {
	freshSinks(t)
	RegisterSink(Sink{Name: "hooks", Kinds: []string{KindUpdated}, Run: nothing})
	cs, seqs := []Change{change("t1", KindUpdated)}, []int64{7}
	if got := eventsFor(context.WithValue(context.Background(), helpers.BulkImportContextKey, true), cs, seqs); len(got) != 0 {
		t.Errorf("a bulk import's change is %+v, want no event", got)
	}
	ctx := helpers.WithAgentWrite(helpers.WithWorkflowGenerated(context.Background()))
	got := eventsFor(ctx, cs, seqs)
	if len(got) != 1 || got[0].Entities["workflow"] != "1" || got[0].Entities["agent_write"] != "1" || got[0].Entities["github"] != "" {
		t.Fatalf("a workflow's agent's change is %+v, want its two marks", got)
	}
	var marks []bool
	freshSinks(t)
	RegisterSink(Sink{Name: "hooks", Kinds: []string{KindUpdated}, Run: func(ctx context.Context, _ string, _ Entry) error {
		marks = []bool{helpers.IsWorkflowGenerated(ctx), helpers.IsAgentWrite(ctx), helpers.IsGitHubOrigin(ctx)}
		return nil
	}})
	l := &fakeLog{next: 7}
	swapIn(t, l)
	Record(context.Background(), change("t1", KindUpdated))
	run := outboxBusiness.Event{EventID: "e7", Entities: got[0].Entities}
	if err := runSinkOf(t, "hooks", run); err != nil || !reflect.DeepEqual(marks, []bool{true, true, false}) {
		t.Errorf("the sink ran with marks %v (err %v), want the workflow's and the agent's alone", marks, err)
	}
}

// The sink is handed the entry as the log keeps it and the event's own id,
// the same on every try. An entry gone since (pruned) is done; a log that
// can't be read is tried again.
func TestASinkReadsItsEntryFromTheLog(t *testing.T) {
	freshSinks(t)
	var ran []Entry
	var ids []string
	RegisterSink(Sink{Name: "hooks", Kinds: []string{KindUpdated}, Run: func(_ context.Context, id string, e Entry) error {
		ran, ids = append(ran, e), append(ids, id)
		return nil
	}})
	l := &fakeLog{next: 41}
	swapIn(t, l)
	kept := Record(context.Background(), change("t1", KindUpdated), change("t2", KindUpdated))
	if err := runSinkOf(t, "hooks", outboxBusiness.Event{EventID: "e42", Entities: map[string]string{"sequence": "42"}}); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || !reflect.DeepEqual(ran[0], kept[1]) || ids[0] != "e42" {
		t.Fatalf("the sink ran with %+v and ids %v, want entry 42 as kept, with e42", ran, ids)
	}
	l.oldest = 41
	if err := runSinkOf(t, "hooks", outboxBusiness.Event{EventID: "e40", Entities: map[string]string{"sequence": "40"}}); err != nil || len(ran) != 1 {
		t.Errorf("a pruned entry ran the sink (%d runs, err %v), want it done", len(ran), err)
	}
	for _, bad := range []string{"", "x", "0"} {
		if err := runSinkOf(t, "hooks", outboxBusiness.Event{Entities: map[string]string{"sequence": bad}}); err != nil || len(ran) != 1 {
			t.Errorf("sequence %q ran the sink (%d runs, err %v), want nothing", bad, len(ran), err)
		}
	}
	gone := errors.New("the database is gone")
	rowsAfter = func(context.Context, int64, int) ([]Row, error) { return nil, gone }
	if err := runSinkOf(t, "hooks", outboxBusiness.Event{Entities: map[string]string{"sequence": "41"}}); !errors.Is(err, gone) {
		t.Errorf("an unreadable log answered %v, want its error, to try again", err)
	}
}

// A sink with no kinds is for nothing, and refused as the Outbox refuses one
// with no name.
func TestASinkNeedsKinds(t *testing.T) {
	freshSinks(t)
	defer func() {
		if recover() == nil {
			t.Error("a sink with no kinds was registered")
		}
	}()
	RegisterSink(Sink{Name: "hooks", Run: nothing})
}

// runSinkOf runs the Outbox sink the log registered as name, as the drain
// would. The Outbox has it: a second sink of that name is refused.
func runSinkOf(t *testing.T, name string, ev outboxBusiness.Event) error {
	t.Helper()
	sinksMu.RLock()
	s, ok := handedTo[name]
	sinksMu.RUnlock()
	if !ok || s.Topic != Topic {
		t.Fatalf("no sink %q of %s: %+v", name, Topic, s)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("the outbox hasn't sink %q of %s", name, Topic)
			}
		}()
		outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: Topic, Name: name, Run: s.Run})
	}()
	return s.Run(context.Background(), ev)
}
