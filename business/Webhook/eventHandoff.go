package business

// Events across a split install.
//
// DispatchEvent hands every workspace event to the listeners registered in
// this process: the Workflow engine, the AI coworker, agent triggers, the
// agent task worker and the Slack bridge. Each registers from a start function
// only a process that runs the background loops calls (cmd/server
// startBackgroundLoops), so with SERVICE_ROLE=api the API process's list was
// empty, and every event a request raised (a message, a reply, a task given to
// an AI teammate) reached no listener anywhere, without a word in any log:
// agents went unanswered, workflows never ran, the Slack bridge stopped
// mirroring (BE-BUG-9).
//
// A process that runs no listeners now hands each event to the workers: one
// entry on a Redis stream (handoffStream), read through a consumer group by
// whichever worker gets it first, run through that worker's listeners and
// acknowledged once every listener has had it. While a worker runs an event
// it keeps the entry claimed; one that dies part way leaves it pending, and
// another takes it over (handoffRetryFirst). A listener that panics is retried
// with backoff, without running again the listeners that already had the
// event, and after handoffAttempts deliveries the entry is set aside on a
// dead-letter stream and logged. Delivery is at least once.
//
// The interim hand-off: the docs programme's Resource Events bus (A6)
// absorbs it. This enqueue becomes Emit, the drain its outbox sweep, and each
// listener a sink on the workers. The entry is shaped for that: kind, when,
// where it came from, and the event's own data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	handoffStream     = "events:handoff"
	handoffDeadStream = "events:handoff:dead"
	handoffGroup      = "listeners"
	// handoffAliveKey is set by every process that drains, and lapses when
	// none does: the system check reads it.
	handoffAliveKey   = "events:handoff:alive"
	handoffDonePrefix = "events:handoff:done:"
)

// The drain's numbers.
var (
	handoffKeep       int64 = 20000 // entries the stream keeps, about
	handoffDeadKeep   int64 = 1000
	handoffRead       int64 = 16
	handoffBlock            = 5 * time.Second
	handoffHeartbeat        = 3 * time.Second
	handoffRetryFirst       = 10 * time.Second
	handoffRetryMax         = 10 * time.Minute
	handoffSweepEvery       = 5 * time.Second
	handoffAttempts   int64 = 6
	handoffAliveFor         = 30 * time.Second
	// handoffWaitMax is the longest a worker waits on one event's listeners
	// before letting the entry go back to the sweep: one that hangs must not
	// hold a slot for ever.
	handoffWaitMax = 10 * time.Minute
	handoffSlots   = 16
)

var handingOff atomic.Bool

// HandEventsToWorkers makes this process hand every event to the workers
// instead of running its own listeners: cmd/server calls it on a process that
// runs no background loops (SERVICE_ROLE=api), which registers none. undo
// turns it off again (tests).
func HandEventsToWorkers() (undo func()) {
	handingOff.Store(true)
	return func() { handingOff.Store(false) }
}

// handedOffEvent is one event as it crosses: what happened, when, the context
// the listeners read (handoffOrigin), and the event's own data, as JSON (a
// list arrives as []interface{}, which every listener reads).
type handedOffEvent struct {
	V      int                    `json:"v"`
	Kind   string                 `json:"kind"`
	At     time.Time              `json:"at"`
	Origin handoffOrigin          `json:"origin"`
	Data   map[string]interface{} `json:"data"`
}

// handoffOrigin is what the listeners read from the dispatching context
// besides the event (business/AIAgent eventAsker): whether its words came
// from outside the workspace, so no member is taken to have asked for what an
// agent does with them. A workflow's or a bulk import's events never get this
// far (DispatchEvent).
type handoffOrigin struct {
	GitHub           bool `json:"github,omitempty"`
	PublicSubmission bool `json:"public_submission,omitempty"`
	AgentWrite       bool `json:"agent_write,omitempty"`
}

func originOf(ctx context.Context) handoffOrigin {
	return handoffOrigin{
		GitHub:           helpers.IsGitHubOrigin(ctx),
		PublicSubmission: helpers.IsPublicSubmission(ctx),
		AgentWrite:       helpers.IsAgentWrite(ctx),
	}
}

// context is the dispatching context's flags again, on a fresh context.
func (o handoffOrigin) context() context.Context {
	ctx := context.Background()
	if o.GitHub {
		ctx = helpers.WithGitHubOrigin(ctx)
	}
	if o.PublicSubmission {
		ctx = helpers.WithPublicSubmission(ctx)
	}
	if o.AgentWrite {
		ctx = helpers.WithAgentWrite(ctx)
	}
	return ctx
}

// handOff sends the event to the workers when this process hands its events
// over, and says whether it did; when it doesn't, the caller runs the
// listeners here.
func handOff(ctx context.Context, eventType string, data map[string]interface{}) bool {
	if !handingOff.Load() {
		return false
	}
	if err := enqueueEvent(ctx, eventType, data); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Webhook could not hand %s to the workers, so no agent, workflow or bridge will see it err: %+v", eventType, err)
	}
	return true
}

func enqueueEvent(ctx context.Context, eventType string, data map[string]interface{}) error {
	body, err := json.Marshal(handedOffEvent{V: 1, Kind: eventType, At: time.Now().UTC(), Origin: originOf(ctx), Data: data})
	if err != nil {
		return err
	}
	c := redisInit.RedisClient
	if c == nil {
		return errors.New("redis isn't connected")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return c.XAdd(ctx, &redis.XAddArgs{
		Stream: handoffStream, MaxLen: handoffKeep, Approx: true,
		Values: map[string]interface{}{"event": string(body)},
	}).Err()
}

// namedListener is a listener with a name that is the same in every process
// of one build, so a worker taking over an event knows which of them had it.
type namedListener struct {
	name string
	fn   EventListener
	// seesRestricted: it is told of a restricted task's events too.
	seesRestricted bool
}

// currentListeners is the listeners registered now, each named by its
// function, a second registration of the same one numbered.
func currentListeners() []namedListener {
	listenerMu.RLock()
	defer listenerMu.RUnlock()
	out := make([]namedListener, 0, len(listeners))
	seen := map[string]int{}
	for i, l := range listeners {
		name := "listener"
		if f := runtime.FuncForPC(reflect.ValueOf(l).Pointer()); f != nil {
			name = f.Name()
		}
		seen[name]++
		if n := seen[name]; n > 1 {
			name += "#" + strconv.Itoa(n)
		}
		out = append(out, namedListener{name: name, fn: l, seesRestricted: i < len(seesRestricted) && seesRestricted[i]})
	}
	return out
}

// handoffBackoff is how long an entry delivered n times waits before its next
// try: handoffRetryFirst, doubling, up to handoffRetryMax.
func handoffBackoff(n int64) time.Duration {
	d := handoffRetryFirst
	for i := int64(1); i < n && d < handoffRetryMax; i++ {
		d *= 2
	}
	return min(d, handoffRetryMax)
}

// StartEventDrain runs this process's listeners on the events api-only
// processes hand over. Started from cmd/server's startBackgroundLoops after
// every listener has registered: an event taken before one had would be
// acknowledged without it.
func StartEventDrain(ctx context.Context) {
	c := redisInit.RedisClient
	if c == nil {
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain: redis isn't connected, so events an api-only server hands over will wait")
		return
	}
	d := &eventDrain{c: c, consumer: drainConsumerName(), inFlight: map[string]bool{}, slots: make(chan struct{}, handoffSlots)}
	helpers.GoSafeLoop("webhook.event-drain", func() { d.run(ctx) })
}

func drainConsumerName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8])
}

type eventDrain struct {
	c        *redis.Client
	consumer string
	mu       sync.Mutex
	inFlight map[string]bool
	slots    chan struct{}
}

func (d *eventDrain) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := d.ensureGroup(ctx); err == nil {
			break
		} else {
			helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not make its group, trying again err: %+v", err)
			sleepCtx(ctx, handoffBlock)
		}
	}
	helpers.GoSafeLoop("d.sweepLoop", func() { d.sweepLoop(ctx) })
	for ctx.Err() == nil {
		res, err := d.c.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: handoffGroup, Consumer: d.consumer,
			Streams: []string{handoffStream, ">"}, Count: handoffRead, Block: handoffBlock,
		}).Result()
		switch {
		case err == nil:
		case errors.Is(err, redis.Nil), ctx.Err() != nil:
			continue
		case strings.HasPrefix(err.Error(), "NOGROUP"):
			// The stream went (Redis was emptied): make it and the group again.
			_ = d.ensureGroup(ctx)
			continue
		default:
			helpers.LogErrorWithContext(ctx, "business/Webhook event drain read err: %+v", err)
			sleepCtx(ctx, handoffBlock) // Redis away: ask again, not in a tight loop
			continue
		}
		for _, s := range res {
			for _, m := range s.Messages {
				d.take(ctx, m)
			}
		}
	}
}

// ensureGroup makes the stream and its group if they aren't there. The group
// starts at the beginning, so events handed over before any worker had ever
// run are taken too.
func (d *eventDrain) ensureGroup(ctx context.Context) error {
	err := d.c.XGroupCreateMkStream(ctx, handoffStream, handoffGroup, "0").Err()
	if err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// take runs m's listeners on a slot of their own, unless this process is
// running it already. The read waits for a slot, so a busy worker takes no
// more than it can run.
func (d *eventDrain) take(ctx context.Context, m redis.XMessage) {
	d.mu.Lock()
	if d.inFlight[m.ID] {
		d.mu.Unlock()
		return
	}
	d.inFlight[m.ID] = true
	d.mu.Unlock()

	d.slots <- struct{}{}
	helpers.GoSafeNamed("business/Webhook/eventHandoff:take", func() {
		defer func() {
			<-d.slots
			d.mu.Lock()
			delete(d.inFlight, m.ID)
			d.mu.Unlock()
		}()
		d.handle(ctx, m)
	})
}

func (d *eventDrain) running(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inFlight[id]
}

// handle runs the listeners that haven't had m yet, and acknowledges it once
// all have. One that panics leaves it pending for the sweep.
func (d *eventDrain) handle(ctx context.Context, m redis.XMessage) {
	raw, _ := m.Values["event"].(string)
	var ev handedOffEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil || ev.Kind == "" {
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain dropped entry %s, which isn't an event err: %v", m.ID, err)
		d.finish(ctx, m.ID)
		return
	}
	stop := d.keepClaimed(ctx, m.ID)
	defer stop()

	doneKey := handoffDonePrefix + m.ID
	done, err := d.c.HGetAll(ctx, doneKey).Result()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not read which listeners had %s, leaving it for later err: %+v", m.ID, err)
		return
	}
	evCtx := ev.Origin.context()
	// A restricted task's events reach only the listeners that may be told
	// of them (RegisterEventListenerSeeingRestricted), as in-process.
	restricted := restrictedTaskEvent(evCtx, ev.Data)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for _, l := range currentListeners() {
		if done[l.name] != "" || (restricted && !l.seesRestricted) {
			continue
		}
		wg.Add(1)
		helpers.GoSafeNamed("business/Webhook/eventHandoff:handle", func() {
			defer wg.Done()
			if !runListener(evCtx, l, ev) {
				failed.Add(1)
				return
			}
			pipe := d.c.TxPipeline()
			pipe.HSet(ctx, doneKey, l.name, "1")
			pipe.Expire(ctx, doneKey, 24*time.Hour)
			if _, err := pipe.Exec(ctx); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not record that %s had %s err: %+v", l.name, m.ID, err)
			}
		})
	}
	all := make(chan struct{})
	helpers.GoSafeNamed("business/Webhook/eventHandoff:handle", func() { wg.Wait(); close(all) })
	select {
	case <-all:
	case <-time.After(handoffWaitMax):
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain: %s's listeners ran past %s, leaving it to the sweep", ev.Kind, handoffWaitMax)
		return
	case <-ctx.Done():
		return
	}
	if failed.Load() > 0 {
		return
	}
	d.finish(ctx, m.ID)
}

// runListener runs one listener, and says whether it returned rather than
// panicked.
func runListener(ctx context.Context, l namedListener, ev handedOffEvent) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "business/Webhook listener %s panicked on %s: %v", l.name, ev.Kind, r)
			ok = false
		}
	}()
	l.fn(ctx, ev.Kind, ev.Data)
	return true
}

// finish acknowledges an entry and forgets which listeners had it.
func (d *eventDrain) finish(ctx context.Context, id string) {
	pipe := d.c.TxPipeline()
	pipe.XAck(ctx, handoffStream, handoffGroup, id)
	pipe.Del(ctx, handoffDonePrefix+id)
	if _, err := pipe.Exec(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not acknowledge %s err: %+v", id, err)
	}
}

// keepClaimed claims id again every handoffHeartbeat while its listeners run,
// so no other worker takes it over as abandoned. Claiming by id alone doesn't
// count as a delivery.
func (d *eventDrain) keepClaimed(ctx context.Context, id string) (stop func()) {
	quit := make(chan struct{})
	helpers.GoSafeLoop("business/Webhook/eventHandoff:keepClaimed", func() {
		t := time.NewTicker(handoffHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = d.c.XClaimJustID(ctx, &redis.XClaimArgs{
					Stream: handoffStream, Group: handoffGroup, Consumer: d.consumer, Messages: []string{id},
				}).Err()
			}
		}
	})
	return func() { close(quit) }
}

// sweepLoop says this process drains, and retries what is pending: an entry
// whose listeners failed, or whose worker died.
func (d *eventDrain) sweepLoop(ctx context.Context) {
	t := time.NewTicker(handoffSweepEvery)
	defer t.Stop()
	for {
		_ = d.c.Set(ctx, handoffAliveKey, d.consumer, handoffAliveFor).Err()
		d.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *eventDrain) sweep(ctx context.Context) {
	pending, err := d.c.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: handoffStream, Group: handoffGroup, Idle: handoffRetryFirst, Start: "-", End: "+", Count: 100,
	}).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) && ctx.Err() == nil && !strings.HasPrefix(err.Error(), "NOGROUP") {
			helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not read what is pending err: %+v", err)
		}
		return
	}
	for _, p := range pending {
		if d.running(p.ID) {
			continue
		}
		if p.RetryCount >= handoffAttempts {
			d.deadLetter(ctx, p.ID, p.RetryCount)
			continue
		}
		wait := handoffBackoff(p.RetryCount)
		if p.Idle < wait {
			continue
		}
		msgs, err := d.c.XClaim(ctx, &redis.XClaimArgs{
			Stream: handoffStream, Group: handoffGroup, Consumer: d.consumer, MinIdle: wait, Messages: []string{p.ID},
		}).Result()
		if err != nil {
			continue
		}
		if len(msgs) == 0 {
			// Trimmed from the stream while it waited: nothing left to run.
			d.finish(ctx, p.ID)
			continue
		}
		for _, m := range msgs {
			d.take(ctx, m)
		}
	}
}

// deadLetter sets an entry aside after its last try: copied to the
// dead-letter stream with how often it was tried, then acknowledged.
func (d *eventDrain) deadLetter(ctx context.Context, id string, tries int64) {
	msgs, err := d.c.XRange(ctx, handoffStream, id, id).Result()
	if err != nil {
		return
	}
	event := ""
	if len(msgs) == 1 {
		event, _ = msgs[0].Values["event"].(string)
	}
	if err := d.c.XAdd(ctx, &redis.XAddArgs{
		Stream: handoffDeadStream, MaxLen: handoffDeadKeep, Approx: true,
		Values: map[string]interface{}{"id": id, "tries": tries, "event": event},
	}).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Webhook event drain could not set %s aside err: %+v", id, err)
		return
	}
	helpers.LogErrorWithContext(ctx, "business/Webhook event %s failed a listener %d times and was set aside on %s", id, tries, handoffDeadStream)
	d.finish(ctx, id)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Is anything taking the events an api-only server hands over? When nothing
// is, every message reaches no agent, workflow or Slack bridge, and nothing
// else says so: the agent queue's own check stays green, since tasks still
// get claimed if their event ever reached a worker.
func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "event-listeners",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Events a request raises (a message, a reply, a task given to an AI teammate) reach the agents, " +
			"workflows and Slack bridge. On a server split into api and workers it checks that a worker is taking " +
			"them; it doesn't say each listener did its work.",
		Probe: probeEventHandoff,
	})
}

func probeEventHandoff(ctx context.Context) error {
	if !handingOff.Load() {
		return nil // this process runs its listeners itself
	}
	c := redisInit.RedisClient
	if c == nil {
		return errors.New("this server hands events to workers through Redis, which isn't connected")
	}
	alive, err := c.Exists(ctx, handoffAliveKey).Result()
	if err != nil {
		return fmt.Errorf("could not read whether a worker is taking events: %w", err)
	}
	if alive == 0 {
		return fmt.Errorf("this server runs as SERVICE_ROLE=api and no worker has taken events in the last %s, "+
			"so messages reach no agent, workflow or Slack bridge. Start a replica with SERVICE_ROLE=worker "+
			"(docker compose --profile workers up -d), or unset SERVICE_ROLE on this one", handoffAliveFor)
	}
	if dead, err := c.XLen(ctx, handoffDeadStream).Result(); err == nil && dead > 0 {
		return helpers.SystemCheckNote(fmt.Sprintf("%d event(s) failed a listener on every try and were set aside "+
			"(Redis stream %s); the server's log names each", dead, handoffDeadStream))
	}
	return nil
}
