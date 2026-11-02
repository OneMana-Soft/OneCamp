package business

// The log's sinks (linear-39): each entry a sink wants goes through the
// Outbox (business/Outbox, PROGRAM.md §4.2 #48), written in the same
// transaction as the entry (appendToStore), so every change the log keeps
// reaches its sinks at least once, after a crash and on whichever process is
// up. The event names the entry by its number, never its body: the sink reads
// the entry from the log when it runs (the log keeps 30 days; the Outbox gives
// up after hours). Webhooks are the first sink (business/Task taskUpdated.go):
// live updates, search and history can follow without a call site of their
// own.

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
)

// Topic is the log's topic in the Outbox.
const Topic = "work.changes"

// Sink is one thing that happens for the log's entries of some kinds.
type Sink struct {
	// Name is stable: the Outbox remembers which sinks had an event by it.
	Name string
	// Kinds are the entries it is for (KindUpdated, ...).
	Kinds []string
	// Run does it, with the Outbox event's id (the same on every try), the
	// entry as the log keeps it, and the writer's marks on ctx (marksOf). An
	// error is tried again later, with the Outbox's backoff; nil is done.
	Run func(ctx context.Context, eventID string, e Entry) error
}

var (
	sinksMu sync.RWMutex
	// sinksOf is the names of the sinks for each kind, in the order they
	// registered; handedTo is each one as the Outbox has it.
	sinksOf  = map[string][]string{}
	handedTo = map[string]outboxBusiness.Sink{}
)

// RegisterSink adds a sink, from its package's init. One with no kinds is for
// nothing and panics, as the Outbox panics at one with no name, no run, or a
// name its topic has already: registration is in init, where that fails the
// tests.
func RegisterSink(s Sink) {
	if len(s.Kinds) == 0 {
		panic(fmt.Sprintf("work feed: sink %q is for no kind of change", s.Name))
	}
	o := outboxBusiness.Sink{Topic: Topic, Name: s.Name, Run: func(ctx context.Context, ev outboxBusiness.Event) error {
		return runSink(ctx, s, ev)
	}}
	if s.Run == nil {
		o.Run = nil
	}
	outboxBusiness.Sinks.Register(o)
	sinksMu.Lock()
	defer sinksMu.Unlock()
	handedTo[s.Name] = o
	for _, k := range s.Kinds {
		sinksOf[k] = append(sinksOf[k], s.Name)
	}
}

// The writer's marks an event carries across, as entities, so its sinks run
// as the write did: a workflow's or an agent's write never wakes a listener
// that would answer it again (helpers.WithWorkflowGenerated), and a listener
// knows whose words it was (business/Webhook handoffOrigin).
var marks = []struct {
	name string
	is   func(context.Context) bool
	with func(context.Context) context.Context
}{
	{"workflow", helpers.IsWorkflowGenerated, helpers.WithWorkflowGenerated},
	{"agent_write", helpers.IsAgentWrite, helpers.WithAgentWrite},
	{"github", helpers.IsGitHubOrigin, helpers.WithGitHubOrigin},
	{"public_submission", helpers.IsPublicSubmission, helpers.WithPublicSubmission},
}

// eventsFor is the Outbox entries for changes just numbered seqs, one for each
// change a sink wants, keyed by what it changed so one task's changes reach a
// sink one at a time, oldest first. A bulk import's changes go to no sink:
// nobody is sent history (business/Webhook DispatchEvent).
func eventsFor(ctx context.Context, changes []Change, seqs []int64) []outboxBusiness.Entry {
	if helpers.IsBulkImport(ctx) || len(changes) != len(seqs) {
		return nil
	}
	sinksMu.RLock()
	defer sinksMu.RUnlock()
	var out []outboxBusiness.Entry
	for i, c := range changes {
		names := sinksOf[c.Kind]
		if len(names) == 0 {
			continue
		}
		entities := map[string]string{"sequence": strconv.FormatInt(seqs[i], 10), c.ResourceKind: c.ResourceUUID, "project": c.ProjectUUID}
		for _, m := range marks {
			if m.is(ctx) {
				entities[m.name] = "1"
			}
		}
		out = append(out, outboxBusiness.Entry{Topic: Topic, Kind: c.Kind, Key: c.ResourceKind + ":" + c.ResourceUUID,
			Entities: entities, Actor: c.Actor, Sinks: append([]string(nil), names...)})
	}
	return out
}

// runSink runs s for the entry ev names, read from the log with the writer's
// marks on ctx. An event naming no entry, or one the log has pruned since, is
// done; a log that can't be read is tried again.
func runSink(ctx context.Context, s Sink, ev outboxBusiness.Event) error {
	seq, err := strconv.ParseInt(ev.Entities["sequence"], 10, 64)
	if err != nil || seq <= 0 {
		return nil
	}
	rows, err := rowsAfter(ctx, seq-1, 1)
	if err != nil {
		return err
	}
	if len(rows) == 0 || rows[0].Sequence != seq {
		return nil
	}
	for _, m := range marks {
		if ev.Entities[m.name] == "1" {
			ctx = m.with(ctx)
		}
	}
	return s.Run(ctx, ev.EventID, entryOf(rows[0]))
}

// handOn releases the events a write's transaction committed and delivers
// them at once; one whose release fails stays held, and the drain releases it
// once its time is up (the topic has no resolver: the entries were committed
// with them). A seam for tests.
var handOn = func(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	if err := outboxBusiness.ReleaseMany(ctx, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/WorkFeed: %d events held until the drain releases them: %v", len(ids), err)
		return
	}
	outboxBusiness.DeliverMany(ids)
}
