// Package business (Outbox) hands the events changes make to the things that
// happen because of them, once each, after a crash, and on whichever process
// is up (PROGRAM.md §4.2 #48; chat A2 builds it, for the message pipeline
// first).
//
// A change writes its event in its own transaction (Add): what it is (topic,
// kind), what it is about (entities: ids by name, never a body: a sink reads
// what is there when it runs, so a message edited since sends its words now,
// and one deleted since is skipped), which of the topic's sinks it is for, and
// the key whose order it keeps (a conversation's messages reach their sinks one
// at a time, oldest first). The row is held until the change's other stores are
// written (a message's graph node) and then released (Release); a writer that
// dies between the two leaves it held, and its topic's resolver decides it.
//
// The drain gives a released row to its topic's sinks, in their order, and
// remembers which had it: a sink that fails is tried again later (backoff),
// without the others running again, and after MaxAttempts tries the row is
// dead, kept for a month and counted by the admins' system check. The writer
// drains the row it released at once (Deliver), so what's live stays live; the
// Sweeper's drain on every worker (outbox.drain, which claims its own rows) is
// the net under it.
//
// Delivery is at least once per sink. A sink makes a repeat harmless itself: an
// upsert by id, a unique row, or FirstTime, a claim per (event, sink,
// recipient) kept past the last retry.
package business

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	models "github.com/akashc777/OneCamp/models/postgres/Outbox"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

// Entry is one event a change made, as it writes it.
type Entry struct {
	Topic string
	Kind  string
	// Key orders the topic's events: those with one key reach their sinks one
	// at a time, oldest first. One waiting out a backoff, or still held by its
	// writer, doesn't hold back those after it.
	Key string
	// Entities are the ids the event is about, by name.
	Entities map[string]string
	// Actor is who made the change, when a person did.
	Actor string
	// Sinks are the topic's sinks it is for; none is all of them.
	Sinks []string
}

// Event is an entry as a sink is handed it.
type Event struct {
	ID       int64
	EventID  string
	Topic    string
	Kind     string
	Key      string
	Entities map[string]string
	Actor    string
	// Attempt is which try this is, from 1.
	Attempt int
}

// Sink is one thing that happens for a topic's events.
type Sink struct {
	Topic string
	// Name is stable: the rows remember which sinks had them by it.
	Name string
	// Order places it among its topic's sinks, lowest first.
	Order int
	// Run does it. An error is tried again later; nil is done, which is also
	// the answer for an event about something gone since.
	Run func(ctx context.Context, ev Event) error
}

// Resolver decides an event its writer never released: whether the change's
// other stores were written (release it) or not (it undoes the change and
// answers false, and the event is cancelled).
type Resolver func(ctx context.Context, ev Event) (written bool, err error)

// The drain's numbers.
const (
	// MaxAttempts is how many tries an event gets before it is dead.
	MaxAttempts = 10
	// lease is how long a claim on a row lasts: a drain that died mid-row
	// leaves it to another once it runs out.
	lease = 2 * time.Minute
	// sinkTimeout bounds one sink's run, inside the lease.
	sinkTimeout = 60 * time.Second
	// drainEvery is the Sweeper's pass.
	drainEvery = 2 * time.Second
	// heldFor is how long a writer has between Add and Release before its
	// topic's resolver is asked.
	heldFor = 30 * time.Second
	// batch is how many rows a pass claims at once; parallel how many run together.
	batch    = 50
	parallel = 8
	// chain is how many rows of a key a drain takes in a row after the first.
	chain = 20
	// doneKeep and deadKeep are how long finished rows are kept: a dead one
	// longer, to be seen.
	doneKeep = 7 * 24 * time.Hour
	deadKeep = 30 * 24 * time.Hour
	// stuckAfter is how long the oldest pending event may wait before the
	// system check says the drain is stuck.
	stuckAfter = 5 * time.Minute
)

// backoff is the wait after each failed try: after the first, backoff[0].
var backoff = []time.Duration{
	10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
	10 * time.Minute, 20 * time.Minute, 30 * time.Minute, time.Hour,
}

// RetryHorizon is the longest an event can take from being written to being
// given up on: held until its resolver is asked (heldFor, then a pass), then
// MaxAttempts tries, each waiting a pass to be claimed and holding its lease to
// the end (a drain that died mid-run), with every backoff between them in
// full. With today's numbers:
//
//	held:      30 s + 2 s                                       =      32 s
//	tries:     10 × (2 s + 2 min)                               = 20 min 20 s
//	backoffs:  10 s + 30 s + 1 + 2 + 5 + 10 + 20 + 30 min + 1 h = 2 h 8 min 40 s
//	in all                                                      = 2 h 29 min 32 s
//
// (TestTheRetryHorizonIsWhatItsCommentSays keeps these numbers honest.) Pure.
func RetryHorizon() time.Duration {
	h := heldFor + drainEvery + time.Duration(MaxAttempts)*(lease+drainEvery)
	for n := 1; n < MaxAttempts; n++ {
		h += helpers.ScheduleWait(backoff, n, 0)
	}
	return h
}

// OnceTTL is how long FirstTime remembers a delivery: past the retry horizon,
// with an hour to spare, so the latest retry an event can have still finds it
// (3 h 29 min 32 s today). A shorter memory would let a late retry send again.
// The hour also covers what the horizon leaves out: an event's turn behind its
// key's earlier ones, each of which runs once in a lease while it waits.
func OnceTTL() time.Duration { return RetryHorizon() + time.Hour }

// FirstTime says whether sink is giving ev to recipient for the first time, and
// remembers that it has for OnceTTL: a sink that tells people (a push, an
// Activity item) asks before each one, so a retry tells only those it hadn't.
// Without Redis, or when it doesn't answer, it says yes: a repeat is better
// than someone never told.
func FirstTime(ctx context.Context, ev Event, sink, recipient string) bool {
	rdb := redisInit.RedisClient
	if rdb == nil {
		return true
	}
	first, err := rdb.SetNX(ctx, registry.OutboxOnce.Build(ev.EventID, sink, recipient), "1", OnceTTL()).Result()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "outbox: whether %s told %s of event %s couldn't be asked, so it tells them: %v", sink, recipient, ev.EventID, err)
		return true
	}
	return first
}

// Told says whether a person is to be told of an event one way ("activity",
// "push", "email") this time. A sink that tells people passes one that asks
// FirstTime, so a retry tells only those it hadn't; nil tells everyone, as a
// write's own fan-out does.
type Told func(how, recipient string) bool

// Only is the recipients t lets through one way: all of them when t is nil.
func (t Told) Only(how string, recipients []string) []string {
	if t == nil {
		return recipients
	}
	out := make([]string, 0, len(recipients))
	for _, r := range recipients {
		if t(how, r) {
			out = append(out, r)
		}
	}
	return out
}

// The store, a seam for tests.
type store interface {
	insert(ctx context.Context, ex models.Execer, rs []models.Row) ([]int64, error)
	release(ctx context.Context, ids []int64) error
	releaseWith(ctx context.Context, id int64, more map[string]string) (bool, error)
	cancel(ctx context.Context, ex models.Execer, id int64) error
	claimDue(ctx context.Context, by string, lease time.Duration, limit int) ([]models.Row, error)
	claim(ctx context.Context, ids []int64, by string, lease time.Duration) ([]models.Row, error)
	claimNextOfKey(ctx context.Context, key string, after int64, by string, lease time.Duration) (models.Row, bool, error)
	finish(ctx context.Context, id int64, by string, done []string) (bool, error)
	retry(ctx context.Context, id int64, by string, done []string, at time.Time, lastErr string, dead bool) (bool, error)
	staleHeld(ctx context.Context, olderThan time.Duration, limit int) ([]models.Row, error)
	purge(ctx context.Context, doneBefore, deadBefore time.Time, limit int) (int64, error)
	stats(ctx context.Context) (models.Stats, error)
}

type pgStore struct{}

func (pgStore) insert(ctx context.Context, ex models.Execer, rs []models.Row) ([]int64, error) {
	return models.Insert(ctx, ex, rs)
}
func (pgStore) release(ctx context.Context, ids []int64) error { return models.Release(ctx, ids) }
func (pgStore) releaseWith(ctx context.Context, id int64, more map[string]string) (bool, error) {
	return models.ReleaseWith(ctx, id, more)
}
func (pgStore) cancel(ctx context.Context, ex models.Execer, id int64) error {
	return models.Cancel(ctx, ex, id)
}
func (pgStore) claimDue(ctx context.Context, by string, l time.Duration, limit int) ([]models.Row, error) {
	return models.ClaimDue(ctx, by, l, limit)
}
func (pgStore) claim(ctx context.Context, ids []int64, by string, l time.Duration) ([]models.Row, error) {
	return models.Claim(ctx, ids, by, l)
}
func (pgStore) claimNextOfKey(ctx context.Context, key string, after int64, by string, l time.Duration) (models.Row, bool, error) {
	return models.ClaimNextOfKey(ctx, key, after, by, l)
}
func (pgStore) finish(ctx context.Context, id int64, by string, done []string) (bool, error) {
	return models.Finish(ctx, id, by, done)
}
func (pgStore) retry(ctx context.Context, id int64, by string, done []string, at time.Time, lastErr string, dead bool) (bool, error) {
	return models.Retry(ctx, id, by, done, at, lastErr, dead)
}
func (pgStore) staleHeld(ctx context.Context, olderThan time.Duration, limit int) ([]models.Row, error) {
	return models.StaleHeld(ctx, olderThan, limit)
}
func (pgStore) purge(ctx context.Context, doneBefore, deadBefore time.Time, limit int) (int64, error) {
	return models.Purge(ctx, doneBefore, deadBefore, limit)
}
func (pgStore) stats(ctx context.Context) (models.Stats, error) { return models.ReadStats(ctx) }

var (
	rows store = pgStore{}
	now        = time.Now
)

// instance names this process in the claims it takes.
var instance = func() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString()[:8])
}()

// Registry holds each topic's sinks and resolver.
type Registry struct {
	mu        sync.RWMutex
	sinks     map[string][]Sink
	resolvers map[string]Resolver
}

// Sinks is the server's one registry. A sink registers from its package's init.
var Sinks = &Registry{}

// Register adds a sink. One with no topic, name or run, or a name its topic
// already has, panics: registration is in init, where that fails the tests.
func (r *Registry) Register(s Sink) {
	if s.Topic == "" || s.Name == "" || s.Run == nil {
		panic(fmt.Sprintf("outbox: sink %q of %q needs a topic, a name and something to run", s.Name, s.Topic))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sinks == nil {
		r.sinks = map[string][]Sink{}
	}
	for _, have := range r.sinks[s.Topic] {
		if have.Name == s.Name {
			panic(fmt.Sprintf("outbox: sink %q of %q is registered twice", s.Name, s.Topic))
		}
	}
	r.sinks[s.Topic] = append(r.sinks[s.Topic], s)
	sort.SliceStable(r.sinks[s.Topic], func(i, j int) bool {
		a, b := r.sinks[s.Topic][i], r.sinks[s.Topic][j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Name < b.Name
	})
}

// Resolve sets a topic's resolver for its held events.
func (r *Registry) Resolve(topic string, res Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolvers == nil {
		r.resolvers = map[string]Resolver{}
	}
	r.resolvers[topic] = res
}

// of is the topic's sinks an event is for, in order: only those named, when
// it names any.
func (r *Registry) of(topic string, only []string) []Sink {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Sink
	for _, s := range r.sinks[topic] {
		if len(only) == 0 || contains(only, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) resolver(topic string) Resolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolvers[topic]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Add writes e as held, inside ex (the change's own transaction), and answers
// its id for Release.
func Add(ctx context.Context, ex models.Execer, e Entry) (int64, error) {
	ids, err := AddMany(ctx, ex, []Entry{e})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

// AddMany writes es as held in one statement, inside ex, and answers their
// ids in es's order for ReleaseMany: what a change that makes many events
// (CreateTasks' new tasks, a table's rows) writes. Every entry is checked
// before anything is written; none writes nothing.
func AddMany(ctx context.Context, ex models.Execer, es []Entry) ([]int64, error) {
	if len(es) == 0 {
		return nil, nil
	}
	rs := make([]models.Row, len(es))
	for i, e := range es {
		if e.Topic == "" || e.Kind == "" || e.Key == "" {
			return nil, errors.New("outbox: an event needs a topic, a kind and a key")
		}
		rs[i] = models.Row{EventID: uuid.New(), Topic: e.Topic, Kind: e.Kind, Key: e.Key, Entities: e.Entities, Sinks: e.Sinks}
		if e.Actor != "" {
			if id, err := uuid.Parse(e.Actor); err == nil {
				rs[i].Actor = &id
			}
		}
	}
	return rows.insert(ctx, ex, rs)
}

// errNoPostgres is an event Enqueue can't write: Postgres isn't connected.
var errNoPostgres = errors.New("outbox: Postgres isn't connected")

// Enqueue writes es ready and delivers them at once: for events no change's
// transaction holds (a job asked for, or a change already saved). It writes
// them in one statement and releases them in one, however many there are.
// Those whose release fails stay held, and the drain releases them once
// their time is up: so it suits topics with no resolver, as on one with a
// resolver (Registry.Resolve) an event left held is that resolver's to
// decide.
func Enqueue(ctx context.Context, es ...Entry) error {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return errNoPostgres
	}
	ids, err := AddMany(ctx, postgresInit.DBConn.SqlDB, es)
	if err != nil {
		return err
	}
	if err := ReleaseMany(ctx, ids); err != nil {
		return nil
	}
	DeliverMany(ids)
	return nil
}

// Release makes a held event ready, once the change's other stores are
// written. It doesn't run the sinks: Deliver does.
func Release(ctx context.Context, id int64) error {
	return ReleaseMany(ctx, []int64{id})
}

// ReleaseMany is Release for the events of ids, in one statement.
func ReleaseMany(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return rows.release(ctx, ids)
}

// ReleaseWith releases a held event as Release does, adding to the ids it is
// about what its change learnt while it wrote its other stores (a DM's first
// message: that it made the conversation, and for whom). A writer that dies
// before releasing leaves the event to its topic's resolver, which releases
// it as it was written.
func ReleaseWith(ctx context.Context, id int64, more map[string]string) error {
	if len(more) == 0 {
		return Release(ctx, id)
	}
	_, err := rows.releaseWith(ctx, id, more)
	return err
}

// Cancel drops an event whose change was undone, inside ex.
func Cancel(ctx context.Context, ex models.Execer, id int64) error {
	return rows.cancel(ctx, ex, id)
}

// Deliver gives a released event to its sinks now, on a goroutine of its own,
// and then the events after it with the same key: what the writer does after
// Release, so a live change reaches open apps at once. One another drain has
// already is left to it; one that fails is the Sweeper's to try again.
func Deliver(id int64) {
	DeliverMany([]int64{id})
}

// DeliverMany is Deliver for the events of ids, claimed a batch at a time
// (one statement each, as the drain claims) and run as the drain runs what it
// claims. A key's events among them still run one at a time, oldest first: a
// later one is taken after the earlier.
func DeliverMany(ids []int64) {
	if len(ids) == 0 {
		return
	}
	helpers.GoSafeNamed("outbox.deliver", func() {
		for from := 0; from < len(ids); from += batch {
			deliverBatch(ids[from:min(from+batch, len(ids))])
		}
	})
}

// deliverBatch claims ids, at most a batch, and runs them inside their own
// lease, so a long list's last ones don't wait out the first ones' lease.
func deliverBatch(ids []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), lease)
	defer cancel()
	claimed, err := rows.claim(ctx, ids, instance, lease)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "outbox: %d events couldn't be claimed to deliver, so the drain will: %v", len(ids), err)
		return
	}
	runClaimed(ctx, "outbox.deliver", claimed)
}

// runClaimed runs claimed rows, each followed by its key's next ones, parallel
// at a time, and waits for them.
func runClaimed(ctx context.Context, name string, claimed []models.Row) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallel)
	for _, r := range claimed {
		slots <- struct{}{}
		wg.Add(1)
		helpers.GoSafeNamed(name, func() {
			defer func() { <-slots; wg.Done() }()
			deliverKey(ctx, r)
		})
	}
	wg.Wait()
}

// deliverKey runs r, then the rows of its key after it, up to chain of them.
func deliverKey(ctx context.Context, r models.Row) {
	for i := 0; ; i++ {
		run(ctx, r)
		if i >= chain || ctx.Err() != nil {
			return
		}
		next, ok, err := rows.claimNextOfKey(ctx, r.Key, r.ID, instance, lease)
		if err != nil || !ok {
			return
		}
		r = next
	}
}

// run gives a claimed row to the sinks that haven't had it, and records what
// happened: done when they all have, tried again later (or dead) when one failed.
func run(ctx context.Context, r models.Row) {
	ev := Event{ID: r.ID, EventID: r.EventID.String(), Topic: r.Topic, Kind: r.Kind, Key: r.Key, Entities: r.Entities, Attempt: r.Attempts}
	if r.Actor != nil {
		ev.Actor = r.Actor.String()
	}
	done := append([]string(nil), r.SinksDone...)
	var failures []string
	for _, s := range Sinks.of(r.Topic, r.Sinks) {
		if contains(done, s.Name) {
			continue
		}
		if err := runSink(ctx, s, ev); err != nil {
			failures = append(failures, s.Name+": "+err.Error())
			continue
		}
		done = append(done, s.Name)
	}
	if len(failures) == 0 {
		if _, err := rows.finish(ctx, r.ID, instance, done); err != nil {
			helpers.LogErrorWithContext(ctx, "outbox: event %d ran but couldn't be marked done, so it may run again: %v", r.ID, err)
		}
		return
	}
	dead := r.Attempts >= MaxAttempts
	why := strings.Join(failures, "; ")
	if dead {
		helpers.LogErrorWithContext(ctx, "outbox: event %d (%s %s, key %s) is given up on after %d tries: %s", r.ID, r.Topic, r.Kind, r.Key, r.Attempts, why)
	}
	if _, err := rows.retry(ctx, r.ID, instance, done, now().Add(helpers.ScheduleWait(backoff, r.Attempts, 0)), why, dead); err != nil {
		helpers.LogErrorWithContext(ctx, "outbox: event %d failed (%s) and couldn't be put back, so its lease will: %v", r.ID, why, err)
	}
}

// runSink runs one sink inside its time, a panic counted as its failure.
func runSink(ctx context.Context, s Sink, ev Event) (err error) {
	sctx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	return s.Run(sctx, ev)
}

// Drain is one pass of the outbox, as the Sweeper runs it on every worker (and
// a test may): held events past their time are decided, then
// due events are claimed and run, a key's in order, several keys at once.
func Drain(ctx context.Context) error {
	resolveHeld(ctx)
	for {
		claimed, err := rows.claimDue(ctx, instance, lease, batch)
		if err != nil {
			return err
		}
		runClaimed(ctx, "outbox.drain", claimed)
		if len(claimed) < batch || ctx.Err() != nil {
			return nil
		}
	}
}

// resolveHeld asks each held event's topic about it, once its writer has had
// its time: released when its change was written, cancelled when it wasn't. A
// topic with no resolver has its held events released: the change was
// committed with them.
func resolveHeld(ctx context.Context) {
	held, err := rows.staleHeld(ctx, heldFor, batch)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "outbox: the held events couldn't be read: %v", err)
		return
	}
	for _, r := range held {
		written := true
		if res := Sinks.resolver(r.Topic); res != nil {
			ev := Event{ID: r.ID, EventID: r.EventID.String(), Topic: r.Topic, Kind: r.Kind, Key: r.Key, Entities: r.Entities}
			ok, err := res(ctx, ev)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "outbox: held event %d couldn't be decided, so it waits: %v", r.ID, err)
				continue
			}
			written = ok
		}
		if written {
			if err := rows.release(ctx, []int64{r.ID}); err != nil {
				helpers.LogErrorWithContext(ctx, "outbox: held event %d couldn't be released: %v", r.ID, err)
			}
			continue
		}
		if err := rows.cancel(ctx, nil, r.ID); err != nil {
			helpers.LogErrorWithContext(ctx, "outbox: held event %d couldn't be cancelled: %v", r.ID, err)
		}
	}
}

// purgePass deletes finished events past their keep, a thousand at a time.
func purgePass(ctx context.Context) error {
	t := now()
	for {
		n, err := rows.purge(ctx, t.Add(-doneKeep), t.Add(-deadKeep), 1000)
		if err != nil || n < 1000 || ctx.Err() != nil {
			return err
		}
	}
}

// probe is the admins' system check: how deep the outbox is (and how much of
// it waits for its writers), how long its oldest pending or held event has
// waited, and how many were given up on and kept.
func probe(ctx context.Context) error {
	s, err := rows.stats(ctx)
	if err != nil {
		return fmt.Errorf("the outbox couldn't be read: %w", err)
	}
	waiting := fmt.Sprintf("%d events wait for their sinks (%d of them for their writers)", s.Pending, s.Held)
	if s.OldestPending > stuckAfter {
		return fmt.Errorf("%s, the oldest for %s: is a worker running the outbox drain?", waiting, s.OldestPending.Round(time.Second))
	}
	if s.Dead > 0 {
		return fmt.Errorf("%d events were given up on after %d tries in the last 30 days; the newest failed with: %s", s.Dead, MaxAttempts, s.LastDead)
	}
	if s.Pending > 0 {
		return helpers.SystemCheckNote(fmt.Sprintf("%s; the oldest has waited %s.", waiting, s.OldestPending.Round(time.Second)))
	}
	return nil
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "outbox.drain", Every: drainEvery, ClaimsItsOwnWork: true, Run: Drain})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "outbox.purge", Every: time.Hour, Run: purgePass})
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "outbox",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Reads the outbox: how many events wait for their sinks (notifications, search, live updates, " +
			"webhooks), how long the oldest has waited, and how many were given up on in the last 30 days. " +
			"It runs no sink itself.",
		Probe: probe,
	})
}
