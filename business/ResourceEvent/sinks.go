package business

// Where events go: the sinks.
//
// A sink registers itself from its package's init(), naming the events it
// takes:
//
//	func init() {
//		resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
//			Name:  "audit",
//			Wants: func(e resourceEventBusiness.Event) bool { return audited[e.Kind] },
//			Run:   recordEvent,
//			InBulk: true,
//		})
//	}
//
// This package imports none of them: the notifications, the audit, the MQTT
// meta events and the webhooks each live with what they need (business/Follow,
// business/Comment, business/AI), and the code that emits imports only this.
//
// HOW A SINK RUNS. Each event a sink wants runs after Emit has returned, on a
// goroutine of its own (helpers.GoSafeNamed), with the emitter's context values
// (who asked, what started it) but not its cancellation, under a time limit. A
// sink runs at most runsPerSink of its events at once; the rest wait their turn,
// in the order they were emitted. A sink that panics or times out is logged and
// holds up no other sink and no later event. Each sink gets each event it wants
// at most once: a run that fails is logged, and not tried again.
//
// BULK IMPORTS. An event from a bulk import (helpers.IsBulkImport) reaches only
// the sinks that say InBulk: an import of a thousand docs must not ring every
// phone or fire a thousand webhooks, while the audit still records what it did.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// sinkTimeLimit bounds one sink's work on one event.
const sinkTimeLimit = 2 * time.Minute

// runsPerSink is how many of its events a sink runs at once. Each event a sink
// wanted ran on a goroutine of its own, all at once, and emitters loop over
// sets of any size (a trash emits once per doc in the subtree): a thousand docs
// started a thousand runs per sink, each reading the graph and writing to a
// Postgres pool of ten connections, with every run's time limit counting while
// it waited for one. Four at a time keeps a burst to a handful of runs per
// sink, and a run is short: a graph read, a row handed to the audit's writer, a
// publish, a webhook handed to its queue.
//
// An event that finds all of its sink's runs busy WAITS its turn in the sink's
// queue, in the order emitted, and its time limit starts when it runs. It is
// never dropped: an audit row or a notice lost to a burst is worse than one a
// little late. And the emitter never waits for it: Emit must not hold up the
// change that caused the event, and a sink that emitted would wait on itself.
// The queue holds the events alone, no goroutine each, so it is as long as the
// burst that filled it; every waitingNote events waiting are logged.
const runsPerSink = 4

// waitingNote is how many events waiting for a sink are worth a line in the
// log, and each as many again after it.
const waitingNote = 1000

// Sink is one place events go.
type Sink struct {
	// Name is unique and says, in the logs, which sink failed.
	Name string
	// Wants says whether the sink takes an event; nil takes every event.
	Wants func(Event) bool
	// Run handles one event. It owns its copy of the event.
	Run func(ctx context.Context, e Event)
	// InBulk is a sink that runs for a bulk import's events too.
	InBulk bool
}

// SinkRegistry holds the sinks.
type SinkRegistry struct {
	mu      sync.RWMutex
	sinks   []*sinkLine
	running helpers.Inflight // every event handed to a sink, waiting or running
}

// sinkLine is a sink and the events waiting for it.
type sinkLine struct {
	Sink
	mu      sync.Mutex
	runs    int       // running now, at most runsPerSink
	waiting []sinkRun // handed over and not started, oldest first
}

// sinkRun is one event for one sink, with the context it runs under.
type sinkRun struct {
	ctx context.Context
	e   Event
}

// Sinks is the server's one registry: Bus fans out to it.
var Sinks = &SinkRegistry{}

// The sinks' runs are background work: a server shutting down and a test
// reading what they wrote wait for them (helpers.SettleBackground).
func init() { helpers.RegisterBackground("events' sinks", Sinks.Settle) }

// Register adds a sink. A sink with no name or nothing to run, or one whose name
// is taken, panics: registration happens in init, where that fails the build's
// tests instead of leaving events that go nowhere.
func (r *SinkRegistry) Register(s Sink) {
	if strings.TrimSpace(s.Name) == "" || s.Run == nil {
		panic(fmt.Sprintf("resource events: sink %q needs a name and something to run", s.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, have := range r.sinks {
		if have.Name == s.Name {
			panic(fmt.Sprintf("resource events: sink %q is registered twice", s.Name))
		}
	}
	r.sinks = append(r.sinks, &sinkLine{Sink: s})
}

// Settle waits until every event handed to a sink so far has been run, or the
// wait is over, and says whether they all were.
func (r *SinkRegistry) Settle(wait time.Duration) bool { return r.running.Settle(wait) }

func (r *SinkRegistry) list() []*sinkLine {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*sinkLine(nil), r.sinks...)
}

// fanOut is the bus: every event to every sink that wants it.
type fanOut struct{ sinks *SinkRegistry }

func (f fanOut) Emit(ctx context.Context, e Event) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e.Kind == "" || e.ResourceKind == "" || e.ResourceUUID == "" {
		helpers.LogErrorWithContext(ctx, "resource events: an event with no kind or resource was dropped: %q %q %q", e.Kind, e.ResourceKind, e.ResourceUUID)
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC()
	bulk := helpers.IsBulkImport(ctx)
	detached := context.WithoutCancel(ctx)
	for _, s := range f.sinks.list() {
		if bulk && !s.InBulk {
			continue
		}
		mine := e.clone()
		if s.Wants != nil && !wants(detached, s.Sink, mine) {
			continue
		}
		f.sinks.running.Add(1)
		f.sinks.hand(s, sinkRun{ctx: detached, e: mine})
	}
}

// wants asks a sink's filter, which must not take the emitter down either.
func wants(ctx context.Context, s Sink, e Event) (yes bool) {
	defer func() {
		if p := recover(); p != nil {
			helpers.LogErrorWithContext(ctx, "resource events: sink %s's filter panicked on %s: %v", s.Name, e.Kind, p)
			yes = false
		}
	}()
	return s.Wants(e)
}

// hand gives a sink one event: it runs now if the sink has a run to spare, and
// otherwise waits its turn (runsPerSink).
func (r *SinkRegistry) hand(s *sinkLine, run sinkRun) {
	s.mu.Lock()
	if s.runs < runsPerSink {
		s.runs++
		s.mu.Unlock()
		r.start(s, run)
		return
	}
	s.waiting = append(s.waiting, run)
	waiting := len(s.waiting)
	s.mu.Unlock()
	if waiting%waitingNote == 0 {
		helpers.LogWarnWithContext(run.ctx, "resource events: %d events are waiting for sink %s", waiting, s.Name)
	}
}

// start runs one event on a goroutine of its own, recovered and named by
// helpers.GoSafeNamed. Done, panicking or not, it hands its place to the
// sink's next event waiting, or gives the place back.
func (r *SinkRegistry) start(s *sinkLine, run sinkRun) {
	e := run.e
	helpers.GoSafeNamed("events: sink "+s.Name+" on "+string(e.Kind)+" for "+e.ResourceKind+" "+e.ResourceUUID, func() {
		defer r.next(s)
		ctx, cancel := context.WithTimeout(run.ctx, sinkTimeLimit)
		defer cancel()
		s.Run(ctx, e)
	})
}

// next ends a run of s: the sink's oldest event waiting takes its place, or,
// with none waiting, the place is given back.
func (r *SinkRegistry) next(s *sinkLine) {
	r.running.Add(-1)
	s.mu.Lock()
	if len(s.waiting) == 0 {
		s.runs--
		s.waiting = nil // a burst's queue goes with it
		s.mu.Unlock()
		return
	}
	run := s.waiting[0]
	s.waiting[0] = sinkRun{}
	s.waiting = s.waiting[1:]
	s.mu.Unlock()
	r.start(s, run)
}

// clone is the event with its own subjects and data, each subject once: a sink
// may keep or change what it was given without touching another sink's.
func (e Event) clone() Event {
	out := e
	out.Subjects = nil
	seen := map[string]bool{"": true}
	for _, s := range e.Subjects {
		s = strings.TrimSpace(s)
		if !seen[s] {
			seen[s] = true
			out.Subjects = append(out.Subjects, s)
		}
	}
	if e.Data != nil {
		out.Data = make(map[string]string, len(e.Data))
		for k, v := range e.Data {
			out.Data[k] = v
		}
	}
	return out
}
