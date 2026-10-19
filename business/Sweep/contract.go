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

import (
	"context"
	"fmt"
	"os"
	"strconv"
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

// claimer says whether this instance runs job's pass for slot. ttl is how long a
// won claim must last.
type claimer func(ctx context.Context, job string, slot int64, ttl time.Duration) bool

// Registry holds the jobs and starts them.
type Registry struct {
	mu      sync.Mutex
	jobs    []Job
	names   map[string]bool
	started context.Context // the context the running jobs were started under
	// claim decides each tick; nil is the Redis claim.
	claim claimer
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
		go every(r.started, j, r.claimer())
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
func (r *Registry) StartAll(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running() {
		return
	}
	r.started = ctx
	for _, j := range r.jobs {
		go every(ctx, j, r.claimer())
	}
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
// each tick whose slot it claims.
func every(ctx context.Context, j Job, claim claimer) {
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
		}
	}
}

// tick runs j for the slot now falls in, when this instance claims it.
func tick(ctx context.Context, j Job, claim claimer, now time.Time) {
	if j.ClaimsItsOwnWork || claim(ctx, j.Name, slotOf(now, j.Every), 2*j.Every) {
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
func claimInRedis(ctx context.Context, job string, slot int64, ttl time.Duration) bool {
	rdb := redisInit.RedisClient
	if rdb == nil {
		return true
	}
	won, err := rdb.SetNX(ctx, registry.SweepClaim.Build(job, strconv.FormatInt(slot, 10)), instance, ttl).Result()
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
