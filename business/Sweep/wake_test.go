package business

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// V-114 (b): a formula made over a big table waited up to a minute for the
// fill sweep's next tick before its fill began, while its reader watched
// "starting". A job someone waits on is woken, and runs at once.

// hub is a shared bus, as Redis's publish and subscribe is for instances: a
// wake published reaches every instance listening.
type hub struct {
	mu        sync.Mutex
	listeners map[int]func(string)
	next      int
	published int
}

func newHub() *hub { return &hub{listeners: map[int]func(string){}} }

func (h *hub) publish(_ context.Context, msg string) error {
	h.mu.Lock()
	h.published++
	got := make([]func(string), 0, len(h.listeners))
	for _, l := range h.listeners {
		got = append(got, l)
	}
	h.mu.Unlock()
	for _, l := range got {
		l(msg)
	}
	return nil
}

func (h *hub) listen(ctx context.Context, got func(string)) {
	h.mu.Lock()
	id := h.next
	h.next++
	h.listeners[id] = got
	h.mu.Unlock()
	<-ctx.Done()
	h.mu.Lock()
	delete(h.listeners, id)
	h.mu.Unlock()
}

func (h *hub) count() (listening, published int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.listeners), h.published
}

// within waits up to d for ok, and fails saying what didn't happen.
func within(t *testing.T, d time.Duration, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", d, what)
		}
	}
}

// stays is n's value a little later: a run that a wake shouldn't start has had
// the time to start.
func stays(n *atomic.Int32) int32 {
	time.Sleep(50 * time.Millisecond)
	return n.Load()
}

// A job that ticks hourly runs as soon as it's woken, in the process that runs
// the sweeps, without a word to the others; each reason wakes it once, and a
// new reason again.
func TestAWokenJobRunsAtOnceNotAtItsNextTick(t *testing.T) {
	bus := newHub()
	r := &Registry{claim: func(context.Context, string, string, time.Duration) bool { return true }, bus: bus}
	var runs atomic.Int32
	r.Register(Job{Name: "tables.computed.recompute", Every: time.Hour, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartAll(ctx)

	r.Wake("tables.computed.recompute", "field 1 at 2")
	within(t, 10*time.Second, func() bool { return runs.Load() == 1 }, "a job that ticks hourly, woken, hasn't run")
	r.Wake("tables.computed.recompute", "field 1 at 2")
	r.Wake("tables.computed.recompute", "field 1 at 2")
	if n := stays(&runs); n != 1 {
		t.Errorf("woken three times for one reason, the job ran %d times, where the reason wakes it once", n)
	}
	r.Wake("tables.computed.recompute", "field 1 at 3")
	within(t, 10*time.Second, func() bool { return runs.Load() == 2 }, "woken for a new reason, the job didn't run again")
	// A read waiting on two fields wakes the job once, when either is new.
	r.Wake("tables.computed.recompute", "field 1 at 3", "field 2 at 1")
	within(t, 10*time.Second, func() bool { return runs.Load() == 3 }, "woken for two reasons, one of them new, the job didn't run")
	r.Wake("tables.computed.recompute", "field 2 at 1", "field 1 at 3")
	if n := stays(&runs); n != 3 {
		t.Errorf("woken again for two reasons it was woken for, the job ran again (%d runs)", n)
	}
	r.Wake("no.such.job", "anything")
	if _, published := bus.count(); published != 0 {
		t.Errorf("a process that runs the sweeps sent %d wakes to the others, where it runs its own", published)
	}
}

// Wakes that come while a pass runs are one more pass after it, never one
// for each wake, and never a pass beside the running one.
func TestWakesWhileAPassRunsAreOneMorePass(t *testing.T) {
	r := &Registry{claim: func(context.Context, string, string, time.Duration) bool { return true }, bus: newHub()}
	var runs, inPass, most atomic.Int32
	release := make(chan struct{})
	r.Register(Job{Name: "tables.computed.recompute", Every: time.Hour, Run: func(context.Context) error {
		if n := inPass.Add(1); n > most.Load() {
			most.Store(n)
		}
		defer inPass.Add(-1)
		if runs.Add(1) == 1 {
			<-release
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartAll(ctx)

	r.Wake("tables.computed.recompute", "first")
	within(t, 10*time.Second, func() bool { return inPass.Load() == 1 }, "the first wake didn't start a pass")
	for i := 0; i < 50; i++ {
		r.Wake("tables.computed.recompute", fmt.Sprintf("while it runs %d", i))
	}
	close(release)
	within(t, 10*time.Second, func() bool { return runs.Load() == 2 }, "fifty wakes during a pass didn't run one more after it")
	if n := stays(&runs); n != 2 {
		t.Errorf("fifty wakes during a pass ran %d passes in all, where they're one more after it", n)
	}
	if most.Load() != 1 {
		t.Errorf("%d passes ran at once in one process, where a wake waits for the running one", most.Load())
	}
}

// A process that runs no sweeps (SERVICE_ROLE=api) hands a wake to the ones
// that do, and one of them runs it: every instance hears the wake, and it is
// claimed like a tick's slot.
func TestAWakeFromAProcessThatRunsNoSweepsRunsOnceOnOneThatDoes(t *testing.T) {
	bus := newHub()
	store := &claims{won: map[string]string{}, asked: map[string]int{}}
	var runs atomic.Int32
	job := func() Job {
		return Job{Name: "tables.computed.recompute", Every: time.Hour, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}}
	}
	api := &Registry{claim: store.for_("api"), bus: bus}
	a := &Registry{claim: store.for_("a"), bus: bus}
	b := &Registry{claim: store.for_("b"), bus: bus}
	for _, r := range []*Registry{api, a, b} {
		r.Register(job())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartAll(ctx)
	b.StartAll(ctx)
	within(t, 10*time.Second, func() bool { n, _ := bus.count(); return n == 2 }, "two instances run the sweeps, and they don't both listen for wakes")

	api.Wake("tables.computed.recompute", "field 1 at 2")
	within(t, 10*time.Second, func() bool { return runs.Load() == 1 }, "a wake from a process that runs no sweeps ran on none of the two that do")
	if n := stays(&runs); n != 1 {
		t.Errorf("one wake ran %d passes across two instances, where one claims it", n)
	}
	api.Wake("tables.computed.recompute", "field 1 at 2")
	if n := stays(&runs); n != 1 {
		t.Errorf("the same reason, given again, ran the job again (%d passes)", n)
	}
	api.Wake("tables.computed.recompute", "field 1 at 3")
	within(t, 10*time.Second, func() bool { return runs.Load() == 2 }, "a new reason, from the same process, didn't run the job again")
	if _, published := bus.count(); published != 2 {
		t.Errorf("the api process sent %d wakes for two reasons", published)
	}
}

// A wake for a job that claims its own work runs on every instance, as its
// ticks do, without asking the claim.
func TestAWakeForAJobThatClaimsItsOwnWorkRunsOnEveryInstance(t *testing.T) {
	bus := newHub()
	var asked, runs atomic.Int32
	never := func(context.Context, string, string, time.Duration) bool { asked.Add(1); return false }
	api, a, b := &Registry{claim: never, bus: bus}, &Registry{claim: never, bus: bus}, &Registry{claim: never, bus: bus}
	for _, r := range []*Registry{api, a, b} {
		r.Register(Job{Name: "content-io.run", Every: time.Hour, ClaimsItsOwnWork: true, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartAll(ctx)
	b.StartAll(ctx)
	within(t, 10*time.Second, func() bool { n, _ := bus.count(); return n == 2 }, "two instances run the sweeps, and they don't both listen for wakes")

	api.Wake("content-io.run", "an export queued")
	within(t, 10*time.Second, func() bool { return runs.Load() == 2 }, "a wake for a job that claims its own work didn't run on both instances")
	if n := asked.Load(); n != 0 {
		t.Errorf("the claim was asked %d times for a job that claims its own work", n)
	}
}
