package business

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

type caught struct {
	mu     sync.Mutex
	events map[string][]Event
}

func (c *caught) sink(name string, wants func(Event) bool, inBulk bool) Sink {
	return Sink{Name: name, Wants: wants, InBulk: inBulk, Run: func(_ context.Context, e Event) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.events[name] = append(c.events[name], e)
	}}
}

func (c *caught) of(name string) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events[name])
}

// Every sink that wants an event gets it once, on its own copy; a sink that
// panics holds up nobody; an event with no resource goes nowhere.
func TestAnEventReachesEverySinkThatWantsIt(t *testing.T) {
	reg := &SinkRegistry{}
	bus := fanOut{sinks: reg}
	c := &caught{events: map[string][]Event{}}
	reg.Register(c.sink("all", nil, false))
	reg.Register(c.sink("renames", func(e Event) bool { return e.Kind == DocRenamed }, false))
	reg.Register(Sink{Name: "breaks", Run: func(context.Context, Event) { panic("a bad sink") }})
	reg.Register(Sink{Name: "breaks its filter", Wants: func(Event) bool { panic("a bad filter") }, Run: func(context.Context, Event) {
		t.Error("a sink whose filter panicked ran")
	}})
	reg.Register(Sink{Name: "changes its copy", Run: func(_ context.Context, e Event) {
		e.Subjects[0] = "someone else"
		e.Data["title"] = "changed"
	}})

	data := map[string]string{"title": "Launch notes"}
	bus.Emit(context.Background(), Event{Kind: DocRenamed, ResourceKind: "doc", ResourceUUID: "d1",
		Subjects: []string{"sam", "", "sam", "maya"}, Data: data})
	bus.Emit(context.Background(), Event{Kind: DocDeleted, ResourceKind: "doc", ResourceUUID: "d1"})
	bus.Emit(context.Background(), Event{Kind: DocDeleted, ResourceKind: "doc"}) // no resource
	if !reg.Settle(5 * time.Second) {
		t.Fatal("the sinks did not finish")
	}

	if got := c.of("all"); len(got) != 2 {
		t.Fatalf("the sink for every event got %d, want 2", len(got))
	}
	renames := c.of("renames")
	if len(renames) != 1 || renames[0].Kind != DocRenamed {
		t.Fatalf("the rename sink got %+v", renames)
	}
	if !slices.Equal(renames[0].Subjects, []string{"sam", "maya"}) {
		t.Errorf("subjects are each once, never empty: %v", renames[0].Subjects)
	}
	if renames[0].Data["title"] != "Launch notes" || data["title"] != "Launch notes" {
		t.Error("a sink that changed its copy changed another's, or the emitter's")
	}
	if renames[0].At.IsZero() || renames[0].At.Location() != time.UTC {
		t.Errorf("an event without a time is stamped now, in UTC: %v", renames[0].At)
	}
}

// Emit returns before any sink runs: a slow sink never holds up the change that
// caused the event.
func TestEmitNeverWaitsForASink(t *testing.T) {
	reg := &SinkRegistry{}
	release := make(chan struct{})
	reg.Register(Sink{Name: "slow", Run: func(context.Context, Event) { <-release }})
	done := make(chan struct{})
	go func() {
		fanOut{sinks: reg}.Emit(context.Background(), Event{Kind: DocUpdated, ResourceKind: "doc", ResourceUUID: "d1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit waited for a sink")
	}
	if reg.Settle(50 * time.Millisecond) {
		t.Error("Settle said a sink still running had finished")
	}
	close(release)
	if !reg.Settle(5 * time.Second) {
		t.Error("the sink did not finish once let go")
	}
}

// A burst of events reaches a sink a few at a time: at most runsPerSink of its
// runs at once, the rest waiting their turn, none dropped. Each event a sink
// wanted ran on a goroutine of its own, all at once: a trash of a thousand docs
// started a thousand runs per sink, against a Postgres pool of ten. In
// synctest's bubble, Wait returns once every goroutine is blocked, so the most
// that ever run at once is seen without a clock.
func TestABurstReachesASinkAFewAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reg := &SinkRegistry{}
		var running, most, ran atomic.Int32
		letGo := make(chan struct{})
		reg.Register(Sink{Name: "slow", Run: func(context.Context, Event) {
			n := running.Add(1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			<-letGo
			running.Add(-1)
			ran.Add(1)
		}})
		bus := fanOut{sinks: reg}
		for i := range 1000 {
			bus.Emit(context.Background(), Event{Kind: DocDeleted, ResourceKind: "doc", ResourceUUID: strconv.Itoa(i)})
		}
		synctest.Wait()
		if got := most.Load(); got != runsPerSink {
			t.Errorf("a burst of 1,000 events ran %d at once in one sink; want %d", got, runsPerSink)
		}
		close(letGo)
		if !reg.Settle(time.Minute) {
			t.Fatal("the sink's runs did not finish once let go")
		}
		if got := ran.Load(); got != 1000 {
			t.Errorf("the sink ran %d of the 1,000 events; want every one", got)
		}
		if got := most.Load(); got != runsPerSink {
			t.Errorf("draining the burst, the sink ran %d at once; want at most %d", got, runsPerSink)
		}
	})
}

// A bulk import's events reach only the sinks that run for imports (the
// audit), never the ones that would ring every phone.
func TestABulkImportReachesOnlyTheSinksForIt(t *testing.T) {
	reg := &SinkRegistry{}
	c := &caught{events: map[string][]Event{}}
	reg.Register(c.sink("notifications", nil, false))
	reg.Register(c.sink("audit", nil, true))
	ctx := context.WithValue(context.Background(), helpers.BulkImportContextKey, true)
	fanOut{sinks: reg}.Emit(ctx, Event{Kind: DocCreated, ResourceKind: "doc", ResourceUUID: "d1"})
	reg.Settle(5 * time.Second)
	if len(c.of("notifications")) != 0 || len(c.of("audit")) != 1 {
		t.Errorf("an import's event reached notifications %d times and the audit %d", len(c.of("notifications")), len(c.of("audit")))
	}
}

// A sink with no name or nothing to run, or a name taken twice, fails where it
// is registered.
func TestABadSinkFailsWhereItIsRegistered(t *testing.T) {
	run := func(context.Context, Event) {}
	for _, c := range []struct {
		name  string
		sinks []Sink
	}{
		{"no name", []Sink{{Run: run}}},
		{"nothing to run", []Sink{{Name: "a"}}},
		{"a name taken", []Sink{{Name: "a", Run: run}, {Name: "a", Run: run}}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registering did not panic", c.name)
				}
			}()
			reg := &SinkRegistry{}
			for _, s := range c.sinks {
				reg.Register(s)
			}
		}()
	}
}

// The bus everything emits to is the fan-out over the server's registry.
func TestTheBusFansOutToTheServersSinks(t *testing.T) {
	f, ok := Bus.(fanOut)
	if !ok || f.sinks != Sinks {
		t.Fatalf("Bus is %T, not the fan-out over Sinks", Bus)
	}
}
