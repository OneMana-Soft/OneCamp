package business

// The Sweeper: periodic jobs that are registered, claimed so that one instance
// runs each pass, and bound to the server's context (docs programme, PROGRAM.md
// §4 #14).
//
// A job registers itself from its package's init():
//
//	func init() {
//		sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "doc.trash.purge", Every: time.Hour, Run: purgeTrash})
//	}
//
// and cmd/server's startBackgroundLoops starts them all, once, with
// Jobs.StartAll(ctx): only in a process whose role runs workers, never on an
// api-only replica. main.go has no other line for sweeps (PROGRAM.md §9).
//
// THE CLAIM. Every worker process ticks every job, and a tick runs the job only
// if it wins the claim for its slot: the stretch of wall-clock time, one interval
// long, that the tick falls in ("doc.trash.purge", 11:00–12:00). The claim is a
// Redis key set once (SET NX) and kept for two intervals, so two instances
// ticking in the same slot run the job once between them, whatever their phase,
// and a slot nobody ticked in is simply skipped. One instance's own ticks are an
// interval apart, so they never meet in one slot. A RunAtStart job also ticks
// when the sweeps start, claimed the same way for the slot the start falls in;
// its first interval tick is in the next slot.
//
// Without Redis (a single instance, or a test) every tick claims. With Redis
// configured but not answering, a tick runs unclaimed, as every job ran before
// the claim existed: a job stays harmless when two processes run it at once (a
// purge finds nothing left to purge), and a sweep that stopped whenever Redis
// blinked would be worse than one that, now and then, runs twice.
//
// WAKES. A job someone is waiting on runs when it's woken (Wake), not at its
// next tick: a formula made over a 100,000-row table waited up to a minute for
// the fill sweep's tick before its fill began, while its reader's view said
// "starting" (V-114 (b)). In a process that runs the sweeps, a wake is a
// signal to the job's own loop, which runs the pass between its ticks, so it
// never runs beside another of its passes there; the loop holds one wake at
// most, so the wakes that come while a pass runs are one more pass after it.
// A process that runs none (SERVICE_ROLE=api) publishes the wake on Redis
// (wakeChannel) to the ones that do; each of them asks for the claim above on
// "wake-" and the wake's id, and the one that wins it runs the pass (every
// one of them, for a job that claims its own work). Each reason wakes a job
// once from a process (why: what its caller waits on, such as one change of a
// field), so a view that asks again every few seconds wakes it once, and a
// pass that can't finish what it was woken for is tried again by the ticks,
// never one pass after another. The ticks run as they always have: a wake
// lost on the way (no Redis, or nobody listening) costs only the wait for the
// next one.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

// Job is one periodic task. Name is unique and stable: the claim is by name.
type Job struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context) error
	// ClaimsItsOwnWork is a job that claims each piece of work itself (a row
	// taken FOR UPDATE SKIP LOCKED, as business/ContentIO's runner does), so
	// every instance runs every pass and they share the work between them.
	ClaimsItsOwnWork bool
	// RunAtStart is a job whose first pass is when the sweeps start rather than
	// one interval later: conversations.index, whose comparison a process that
	// has just started waits for before it trusts the conversations index
	// (business/Conversation index.go).
	RunAtStart bool
}

// claimer says whether this instance runs job's pass for slot: a tick's slot,
// or "wake-" and a wake's id. ttl is how long a won claim must last.
type claimer func(ctx context.Context, job string, slot string, ttl time.Duration) bool

// Registry holds the jobs and starts them.
type Registry struct {
	mu      sync.Mutex
	jobs    []Job
	names   map[string]bool
	started context.Context // the context the running jobs were started under
	// claim decides each tick and each wake from another instance; nil is the
	// Redis claim.
	claim claimer
	// bus carries wakes between instances; nil is Redis's (redisWakes).
	bus wakeBus
	// wakes are the jobs' wakes, by name, each holding one at most.
	wakes map[string]chan struct{}
	// woken is the reasons this process has woken its jobs for, the latest
	// wakeMemory of them.
	woken recent
}

// wakeBus carries a wake from a process that runs no sweeps to the ones that
// do: Redis's publish and subscribe, or a test's own.
type wakeBus interface {
	publish(ctx context.Context, msg string) error
	// listen hands got each wake sent, until ctx is done.
	listen(ctx context.Context, got func(msg string))
}

const (
	// wakeMemory is how many reasons a process remembers having woken its
	// jobs for: past it, the oldest is forgotten, and given again it wakes
	// its job once more.
	wakeMemory = 4096
	// wakeSendFor is how long a wake has to reach Redis.
	wakeSendFor = 5 * time.Second
	// wakeClaimFor is how long a wake's claim lasts: past the moment every
	// process that runs the sweeps has heard the wake and asked for it.
	wakeClaimFor = time.Minute
	// wakeChannel is the Redis channel wakes are published on.
	wakeChannel = "sweep:wake"
)

// Wake asks for a pass of the job named now, rather than at its next tick, for
// whys: what the caller waits on, such as the changes of the fields a read
// waits for. A reason wakes the job once from this process, however often
// it's given, so the job is woken when any of whys is new; with none, every
// time. A name no job has wakes nothing. Wake never waits:
// in a process that runs the sweeps the job's own loop runs the pass as soon
// as it is free, and from one that runs none the wake is published on Redis
// in the background, where one of the processes that run the sweeps claims
// it (see the file's comment).
func (r *Registry) Wake(name string, whys ...string) {
	r.mu.Lock()
	fresh := len(whys) == 0
	for _, why := range whys {
		if r.names[name] && r.woken.add(name+"\n"+why) {
			fresh = true
		}
	}
	if !r.names[name] || !fresh {
		r.mu.Unlock()
		return
	}
	wake, here, bus := r.wakeOf(name), r.running(), r.wakeBus()
	r.mu.Unlock()
	if here {
		signal(wake)
		return
	}
	msg := name + "\n" + uuid.NewString()
	helpers.GoSafeNamed("sweep:wake:"+name, func() {
		ctx, cancel := context.WithTimeout(context.Background(), wakeSendFor)
		defer cancel()
		if err := bus.publish(ctx, msg); err != nil {
			helpers.LogErrorWithContext(ctx, "sweep %s: the wake couldn't be published, so the job's next tick runs it: %v", name, err)
		}
	})
}

// woke is a wake another process published: this one runs the pass when the
// job is one of its own and it wins the wake's claim, or, for a job that
// claims its own work, at once.
func (r *Registry) woke(ctx context.Context, claim claimer, msg string) {
	name, id, ok := strings.Cut(msg, "\n")
	if !ok || id == "" {
		return
	}
	r.mu.Lock()
	known, own := false, false
	for _, j := range r.jobs {
		if j.Name == name {
			known, own = true, j.ClaimsItsOwnWork
		}
	}
	wake := r.wakeOf(name)
	r.mu.Unlock()
	if known && (own || claim(ctx, name, "wake-"+id, wakeClaimFor)) {
		signal(wake)
	}
}

// wakeOf is job name's wake, made the first time it's asked for. Called with
// mu held.
func (r *Registry) wakeOf(name string) chan struct{} {
	if r.wakes == nil {
		r.wakes = map[string]chan struct{}{}
	}
	w, ok := r.wakes[name]
	if !ok {
		w = make(chan struct{}, 1)
		r.wakes[name] = w
	}
	return w
}

func (r *Registry) wakeBus() wakeBus {
	if r.bus != nil {
		return r.bus
	}
	return redisWakes{}
}

// signal leaves a wake in w, unless one is waiting there already.
func signal(w chan struct{}) {
	select {
	case w <- struct{}{}:
	default:
	}
}

// recent is a set of the latest wakeMemory strings added to it.
type recent struct {
	in    map[string]bool
	order []string
	next  int
}

// add puts s in the set, saying whether it wasn't there.
func (c *recent) add(s string) bool {
	if c.in[s] {
		return false
	}
	if c.in == nil {
		c.in, c.order = make(map[string]bool, wakeMemory), make([]string, wakeMemory)
	}
	if old := c.order[c.next]; old != "" {
		delete(c.in, old)
	}
	c.order[c.next], c.in[s] = s, true
	c.next = (c.next + 1) % wakeMemory
	return true
}

// redisWakes is the wakes' bus between processes: Redis's publish and
// subscribe on wakeChannel. With no Redis configured, nothing is published,
// and there's nothing to listen to.
type redisWakes struct{}

func (redisWakes) publish(ctx context.Context, msg string) error {
	rdb := redisInit.RedisClient
	if rdb == nil {
		return nil
	}
	return rdb.Publish(ctx, wakeChannel, msg).Err()
}

// listen subscribes until ctx is done; the client subscribes again by itself
// after a lost connection.
func (redisWakes) listen(ctx context.Context, got func(string)) {
	rdb := redisInit.RedisClient
	if rdb == nil {
		return
	}
	sub := rdb.Subscribe(ctx, wakeChannel)
	defer sub.Close()
	msgs := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			got(m.Payload)
		}
	}
}

// Jobs is the server's one registry.
var Jobs = &Registry{}

// Register adds a job. A job with no name, no interval or nothing to run, or one
// whose name is taken, panics: registration happens in init, where that fails the
// build's tests instead of leaving a sweep that never runs. A job registered after
// StartAll starts at once.
func (r *Registry) Register(j Job) {
	if j.Name == "" || j.Every <= 0 || j.Run == nil {
		panic(fmt.Sprintf("sweep: job %q needs a name, an interval and something to run", j.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = map[string]bool{}
	}
	if r.names[j.Name] {
		panic(fmt.Sprintf("sweep: job %q is registered twice", j.Name))
	}
	r.names[j.Name] = true
	r.jobs = append(r.jobs, j)
	if r.running() {
		started, claim, wake := r.started, r.claimer(), r.wakeOf(j.Name)
		helpers.GoSafeLoop("sweep:"+j.Name, func() { every(started, j, claim, wake) })
	}
}

// StartAll starts every registered job, bound to ctx: each stops when ctx is
// done. While the jobs an earlier call started are running, calling it again
// does nothing. Once that call's context is done they have stopped with it, and
// a new call starts them again under the new context: a registry whose jobs had
// all stopped would otherwise never run them again in that process. (The
// integration tests start the sweeps under each test's own context, and after
// one test's context ended, the content queue was never read again in that
// process, so a later test's workspace export sat queued until it gave up.)
// It also listens, as long, for the wakes processes that run no sweeps publish.
func (r *Registry) StartAll(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running() {
		return
	}
	r.started = ctx
	for _, j := range r.jobs {
		claim, wake := r.claimer(), r.wakeOf(j.Name)
		helpers.GoSafeLoop("sweep:"+j.Name, func() { every(ctx, j, claim, wake) })
	}
	bus, claim := r.wakeBus(), r.claimer()
	helpers.GoSafeLoop("sweep:wakes", func() {
		bus.listen(ctx, func(msg string) { r.woke(ctx, claim, msg) })
	})
}

// running says whether the jobs are running: started, under a context that
// hasn't ended. Called with mu held.
func (r *Registry) running() bool {
	return r.started != nil && r.started.Err() == nil
}

func (r *Registry) claimer() claimer {
	if r.claim != nil {
		return r.claim
	}
	return claimInRedis
}

// every ticks j once per interval, the first time one interval after the start
// (and at the start too for a RunAtStart job), until ctx is done, and runs it on
// each tick whose slot it claims, and whenever it's woken (Wake: the wake was
// claimed already when another process published it).
func every(ctx context.Context, j Job, claim claimer, wake <-chan struct{}) {
	if j.RunAtStart && ctx.Err() == nil {
		tick(ctx, j, claim, time.Now())
	}
	t := time.NewTicker(j.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			tick(ctx, j, claim, now)
		case <-wake:
			if ctx.Err() == nil {
				runOnce(ctx, j)
			}
		}
	}
}

// tick runs j for the slot now falls in, when this instance claims it.
func tick(ctx context.Context, j Job, claim claimer, now time.Time) {
	if j.ClaimsItsOwnWork || claim(ctx, j.Name, strconv.FormatInt(slotOf(now, j.Every), 10), 2*j.Every) {
		runOnce(ctx, j)
	}
}

// slotOf is the slot a tick at now belongs to: which interval since the Unix
// epoch it falls in. Pure.
func slotOf(now time.Time, every time.Duration) int64 {
	return now.UnixNano() / int64(every)
}

// instance names this process in the claims it wins, for whoever reads Redis.
var instance = func() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString()[:8])
}()

// claimInRedis is the claim across instances (see the file's comment).
func claimInRedis(ctx context.Context, job string, slot string, ttl time.Duration) bool {
	rdb := redisInit.RedisClient
	if rdb == nil {
		return true
	}
	won, err := rdb.SetNX(ctx, registry.SweepClaim.Build(job, slot), instance, ttl).Result()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "sweep %s: the claim couldn't be asked for, so this pass runs unclaimed: %v", job, err)
		return true
	}
	return won
}

// runOnce runs a job, logging what it returns. A panic is logged and the job
// keeps its schedule: one bad pass never ends the sweep for good.
func runOnce(ctx context.Context, j Job) {
	defer func() {
		if p := recover(); p != nil {
			helpers.LogErrorWithContext(ctx, "sweep %s panicked: %v", j.Name, p)
		}
	}()
	if err := j.Run(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "sweep %s failed: %v", j.Name, err)
	}
}
