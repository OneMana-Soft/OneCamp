package business

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/Outbox"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

// fakeRows is the outbox table as the drain sees it, in memory: rows, their
// states and claims, and the claim's rules (due, unclaimed, a key's oldest).
type fakeRows struct {
	mu    sync.Mutex
	rows  []*models.Row
	state map[int64]*fakeState
	clock time.Time
	now   models.Stats
	// The statements a writer's path and its delivery cost.
	inserts, releases, claims int
}

type fakeState struct {
	available time.Time
	claimedBy string
	until     time.Time
	lastErr   string
	created   time.Time
}

func newFake() *fakeRows {
	return &fakeRows{state: map[int64]*fakeState{}, clock: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeRows) row(id int64) *models.Row {
	for _, r := range f.rows {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *fakeRows) insert(_ context.Context, _ models.Execer, rs []models.Row) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts++
	ids := make([]int64, 0, len(rs))
	for _, r := range rs {
		r.ID = int64(len(f.rows) + 1)
		r.State = models.StateHeld
		r.CreatedAt = f.clock
		f.rows = append(f.rows, &r)
		f.state[r.ID] = &fakeState{available: f.clock, created: f.clock}
		ids = append(ids, r.ID)
	}
	return ids, nil
}

func (f *fakeRows) release(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	for _, id := range ids {
		if r := f.row(id); r != nil && r.State == models.StateHeld {
			r.State = models.StateReady
			f.state[id].available = f.clock
		}
	}
	return nil
}

func (f *fakeRows) releaseWith(ctx context.Context, id int64, more map[string]string) (bool, error) {
	f.mu.Lock()
	r := f.row(id)
	held := r != nil && r.State == models.StateHeld
	if held {
		merged := map[string]string{}
		for k, v := range r.Entities {
			merged[k] = v
		}
		for k, v := range more {
			merged[k] = v
		}
		r.Entities = merged
	}
	f.mu.Unlock()
	return held, f.release(ctx, []int64{id})
}

func (f *fakeRows) cancel(_ context.Context, _ models.Execer, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.row(id); r != nil && (r.State == models.StateHeld || r.State == models.StateReady) {
		r.State = models.StateCancelled
	}
	return nil
}

// claimable is the SQL's condition, in memory. Called with mu held.
func (f *fakeRows) claimable(r *models.Row) bool {
	s := f.state[r.ID]
	if r.State != models.StateReady || s.available.After(f.clock) || (s.claimedBy != "" && !s.until.Before(f.clock)) {
		return false
	}
	for _, p := range f.rows {
		if p.Key == r.Key && p.ID < r.ID && p.State == models.StateReady && !f.state[p.ID].available.After(f.clock) {
			return false
		}
	}
	return true
}

func (f *fakeRows) take(r *models.Row, by string, l time.Duration) models.Row {
	s := f.state[r.ID]
	s.claimedBy, s.until = by, f.clock.Add(l)
	r.Attempts++
	return *r
}

func (f *fakeRows) claimDue(_ context.Context, by string, l time.Duration, limit int) ([]models.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.Row
	keys := map[string]bool{}
	for _, r := range f.rows {
		if len(out) == limit {
			break
		}
		if keys[r.Key] || !f.claimable(r) {
			continue
		}
		keys[r.Key] = true
		out = append(out, f.take(r, by, l))
	}
	return out, nil
}

func (f *fakeRows) claim(_ context.Context, ids []int64, by string, l time.Duration) ([]models.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	var out []models.Row
	for _, id := range ids {
		if r := f.row(id); r != nil && f.claimable(r) {
			out = append(out, f.take(r, by, l))
		}
	}
	return out, nil
}

func (f *fakeRows) claimNextOfKey(_ context.Context, key string, after int64, by string, l time.Duration) (models.Row, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Key == key && r.ID > after && f.claimable(r) {
			return f.take(r, by, l), true, nil
		}
	}
	return models.Row{}, false, nil
}

func (f *fakeRows) finish(_ context.Context, id int64, by string, done []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, s := f.row(id), f.state[id]
	if r == nil || s.claimedBy != by {
		return false, nil
	}
	r.State, r.SinksDone, s.claimedBy = models.StateDone, done, ""
	return true, nil
}

func (f *fakeRows) retry(_ context.Context, id int64, by string, done []string, at time.Time, lastErr string, dead bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, s := f.row(id), f.state[id]
	if r == nil || s.claimedBy != by {
		return false, nil
	}
	r.SinksDone, s.claimedBy, s.available, s.lastErr = done, "", at, lastErr
	r.State = models.StateReady
	if dead {
		r.State = models.StateDead
	}
	return true, nil
}

func (f *fakeRows) staleHeld(_ context.Context, olderThan time.Duration, limit int) ([]models.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.Row
	for _, r := range f.rows {
		if r.State == models.StateHeld && f.state[r.ID].created.Before(f.clock.Add(-olderThan)) && len(out) < limit {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeRows) purge(context.Context, time.Time, time.Time, int) (int64, error) { return 0, nil }
func (f *fakeRows) stats(context.Context) (models.Stats, error)                     { return f.now, nil }

func (f *fakeRows) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

// statements is what the writes and claims cost so far.
func (f *fakeRows) statements() (inserts, releases, claims int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserts, f.releases, f.claims
}

func (f *fakeRows) stateOf(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.row(id).State
}

// withFake swaps the store, the clock and the registry for the test's own.
func withFake(t *testing.T) *fakeRows {
	t.Helper()
	f := newFake()
	prevRows, prevNow, prevSinks := rows, now, Sinks
	rows, Sinks = f, &Registry{}
	now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}
	t.Cleanup(func() { rows, now, Sinks = prevRows, prevNow, prevSinks })
	return f
}

// recorder is sinks that note each event they're given, in order.
type recorder struct {
	mu  sync.Mutex
	ran []string
}

func (r *recorder) sink(topic, name string, order int, fail func(ev Event) error) Sink {
	return Sink{Topic: topic, Name: name, Order: order, Run: func(_ context.Context, ev Event) error {
		r.mu.Lock()
		r.ran = append(r.ran, name+"#"+ev.Entities["message"])
		r.mu.Unlock()
		if fail != nil {
			return fail(ev)
		}
		return nil
	}}
}

func (r *recorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

func add(t *testing.T, key, message string, only ...string) int64 {
	t.Helper()
	id, err := Add(context.Background(), nil, Entry{Topic: "test", Kind: "test.made", Key: key, Entities: map[string]string{"message": message}, Sinks: only})
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTheRetryHorizonIsWhatItsCommentSays(t *testing.T) {
	if got, want := RetryHorizon(), 2*time.Hour+29*time.Minute+32*time.Second; got != want {
		t.Errorf("RetryHorizon() = %s, the comment says %s", got, want)
	}
	if got, want := OnceTTL(), 3*time.Hour+29*time.Minute+32*time.Second; got != want {
		t.Errorf("OnceTTL() = %s, the comment says %s", got, want)
	}
	// The claim's memory outlasts every retry, and the key's documented TTL covers it.
	if OnceTTL() <= RetryHorizon() {
		t.Error("a late retry could find its claim gone and tell someone twice")
	}
	if registry.OutboxOnce.TTL < OnceTTL() {
		t.Errorf("registry.OutboxOnce.TTL %s is shorter than FirstTime's %s", registry.OutboxOnce.TTL, OnceTTL())
	}
}

func TestAnEventReachesEachOfItsSinksOnceInTheirOrder(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "search", 20, nil))
	Sinks.Register(rec.sink("test", "realtime", 10, nil))
	Sinks.Register(rec.sink("other", "unrelated", 0, nil))
	id := add(t, "channel:c1", "m1")
	if err := Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1,search#m1" {
		t.Errorf("ran %s, want realtime then search, once each", got)
	}
	if s := f.stateOf(id); s != models.StateDone {
		t.Errorf("state %s, want done", s)
	}
	// Another pass finds nothing to do.
	_ = Drain(context.Background())
	if n := len(rec.calls()); n != 2 {
		t.Errorf("%d runs after a second pass, want 2", n)
	}
}

func TestAFailedSinkIsTriedAgainAloneAfterItsBackoff(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	tries := 0
	Sinks.Register(rec.sink("test", "realtime", 10, nil))
	Sinks.Register(rec.sink("test", "notify", 20, func(Event) error {
		tries++
		if tries == 1 {
			return errors.New("FCM said 503")
		}
		return nil
	}))
	id := add(t, "dm:a b", "m1")
	_ = Drain(context.Background())
	if s := f.stateOf(id); s != models.StateReady {
		t.Fatalf("state %s after a failure, want ready (to try again)", s)
	}
	// Not before its backoff.
	_ = Drain(context.Background())
	if n := len(rec.calls()); n != 2 {
		t.Fatalf("%d runs before the backoff ran out, want 2", n)
	}
	f.advance(helpers.ScheduleWait(backoff, 1, 0))
	_ = Drain(context.Background())
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1,notify#m1,notify#m1" {
		t.Errorf("ran %s: the retry gave the event to the sink that had it", got)
	}
	if s := f.stateOf(id); s != models.StateDone {
		t.Errorf("state %s, want done", s)
	}
}

func TestAnEventIsGivenUpOnAfterItsLastTryAndKept(t *testing.T) {
	f := withFake(t)
	Sinks.Register(Sink{Topic: "test", Name: "webhook", Run: func(context.Context, Event) error { return errors.New("hook answered 500") }})
	id := add(t, "channel:c1", "m1")
	for i := 0; i < MaxAttempts+2; i++ {
		_ = Drain(context.Background())
		f.advance(time.Hour)
	}
	if s := f.stateOf(id); s != models.StateDead {
		t.Fatalf("state %s after %d tries, want dead", s, MaxAttempts)
	}
	f.mu.Lock()
	attempts, why := f.row(id).Attempts, f.state[id].lastErr
	f.mu.Unlock()
	if attempts != MaxAttempts || !strings.Contains(why, "hook answered 500") {
		t.Errorf("attempts %d, error %q: it should stop at %d with the sink's words", attempts, why, MaxAttempts)
	}
}

func TestAnEventForSomeSinksReachesOnlyThem(t *testing.T) {
	withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 10, nil))
	Sinks.Register(rec.sink("test", "search", 20, nil))
	Sinks.Register(rec.sink("test", "notify", 30, nil))
	add(t, "group:g1", "copy1", "search")
	_ = Drain(context.Background())
	if got := strings.Join(rec.calls(), ","); got != "search#copy1" {
		t.Errorf("ran %s, want only search", got)
	}
}

func TestASinkThatPanicsFailsAloneAndIsTriedAgain(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	Sinks.Register(Sink{Topic: "test", Name: "broken", Order: 1, Run: func(context.Context, Event) error { panic("nil map") }})
	Sinks.Register(rec.sink("test", "realtime", 2, nil))
	id := add(t, "channel:c1", "m1")
	_ = Drain(context.Background())
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1" {
		t.Errorf("ran %s: the sink after a panicking one should still run", got)
	}
	f.mu.Lock()
	why, done := f.state[id].lastErr, f.row(id).SinksDone
	f.mu.Unlock()
	if !strings.Contains(why, "panicked: nil map") || len(done) != 1 || done[0] != "realtime" {
		t.Errorf("error %q, done %v", why, done)
	}
}

func TestAKeysEventsRunOneAtATimeOldestFirst(t *testing.T) {
	f := withFake(t)
	var mu sync.Mutex
	running, most := 0, 0
	var order []string
	started, release := make(chan struct{}), make(chan struct{})
	Sinks.Register(Sink{Topic: "test", Name: "realtime", Run: func(_ context.Context, ev Event) error {
		mu.Lock()
		running++
		if running > most {
			most = running
		}
		order = append(order, ev.Entities["message"])
		mu.Unlock()
		if ev.Entities["message"] == "m1" {
			close(started)
			<-release
		}
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}})
	add(t, "channel:c1", "m1")
	add(t, "channel:c1", "m2")
	add(t, "channel:c2", "x1")
	done := make(chan struct{})
	go func() { _ = Drain(context.Background()); close(done) }()
	// While m1 runs, a second drain takes nothing of its key.
	<-started
	more, _ := f.claimDue(context.Background(), "other", lease, batch)
	for _, r := range more {
		if r.Key == "channel:c1" {
			t.Errorf("a second drain claimed %d of channel:c1 while its first event ran", r.ID)
		}
	}
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	at := map[string]int{}
	for i, m := range order {
		at[m] = i
	}
	if len(order) != 3 || at["m1"] > at["m2"] {
		t.Errorf("order %v: each once, and m2 after m1", order)
	}
	if most > 2 {
		t.Errorf("%d ran at once: one key at a time, two keys here", most)
	}
}

func TestAHeldEventIsDecidedByItsTopicOnceItsWriterHadItsTime(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 1, nil))
	written := map[string]bool{"m1": true, "m2": false}
	Sinks.Resolve("test", func(_ context.Context, ev Event) (bool, error) {
		if ev.Entities["message"] == "m3" {
			return false, errors.New("graph down")
		}
		return written[ev.Entities["message"]], nil
	})
	ids := map[string]int64{}
	for _, m := range []string{"m1", "m2", "m3"} {
		id, err := Add(context.Background(), nil, Entry{Topic: "test", Kind: "test.made", Key: "k-" + m, Entities: map[string]string{"message": m}})
		if err != nil {
			t.Fatal(err)
		}
		ids[m] = id
	}
	// Within the writer's time, nothing is decided.
	_ = Drain(context.Background())
	if f.stateOf(ids["m1"]) != models.StateHeld {
		t.Fatal("a held event was decided before its writer had its time")
	}
	f.advance(heldFor + time.Second)
	_ = Drain(context.Background())
	if got := []string{f.stateOf(ids["m1"]), f.stateOf(ids["m2"]), f.stateOf(ids["m3"])}; got[0] != models.StateReady && got[0] != models.StateDone || got[1] != models.StateCancelled || got[2] != models.StateHeld {
		t.Errorf("states %v: written is released, unwritten cancelled, undecided left held", got)
	}
	_ = Drain(context.Background())
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1" {
		t.Errorf("ran %s: only the written one reaches its sinks", got)
	}
}

func TestDeliverRunsTheReleasedEventAndTheKeysNextAtOnce(t *testing.T) {
	withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 1, nil))
	second := make(chan struct{})
	Sinks.Register(Sink{Topic: "test", Name: "watch", Order: 2, Run: func(_ context.Context, ev Event) error {
		if ev.Entities["message"] == "m2" {
			close(second)
		}
		return nil
	}})
	first := add(t, "channel:c1", "m1")
	add(t, "channel:c1", "m2")
	Deliver(first)
	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("the key's next event never ran")
	}
	// The delivery finishes before the test puts the store back.
	helpers.SettleBackground(10 * time.Second)
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1,realtime#m2" {
		t.Errorf("ran %s: the writer's delivery should take the key's next event too", got)
	}
}

// The writers' deliveries keep a key's order too: the later message's writer
// delivering first takes nothing while the earlier one is due, and the earlier
// one's delivery takes both, in order.
func TestALaterEventsDeliveryWaitsForItsKeysEarlierOne(t *testing.T) {
	withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 1, nil))
	m1 := add(t, "channel:c1", "m1")
	m2 := add(t, "channel:c1", "m2")
	Deliver(m2)
	helpers.SettleBackground(10 * time.Second)
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("ran %v while the key's earlier event waited", got)
	}
	Deliver(m1)
	helpers.SettleBackground(10 * time.Second)
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1,realtime#m2" {
		t.Errorf("ran %s: m1, then m2", got)
	}
}

// An event waiting out its backoff doesn't hold back the ones after it: the
// broker being away for one message doesn't stop the conversation.
func TestAnEventInBackoffDoesntHoldBackItsKeysNext(t *testing.T) {
	f := withFake(t)
	failed := false
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 1, func(ev Event) error {
		if ev.Entities["message"] == "m1" && !failed {
			failed = true
			return errors.New("the broker is away")
		}
		return nil
	}))
	m1 := add(t, "channel:c1", "m1")
	m2 := add(t, "channel:c1", "m2")
	Deliver(m1)
	helpers.SettleBackground(10 * time.Second)
	if got := strings.Join(rec.calls(), ","); got != "realtime#m1,realtime#m2" {
		t.Errorf("ran %s: m1 failed, and m2 goes on", got)
	}
	if f.stateOf(m1) != models.StateReady || f.stateOf(m2) != models.StateDone {
		t.Errorf("m1 %s, m2 %s: m1 waits to be tried again, m2 is done", f.stateOf(m1), f.stateOf(m2))
	}
	f.advance(helpers.ScheduleWait(backoff, 1, 0) + time.Second)
	_ = Drain(context.Background())
	if f.stateOf(m1) != models.StateDone {
		t.Errorf("m1 %s after its backoff: tried again and done", f.stateOf(m1))
	}
}

func TestTheSystemCheckSaysWhatAnAdminNeedsToKnow(t *testing.T) {
	f := withFake(t)
	cases := []struct {
		stats models.Stats
		want  string
		note  bool
	}{
		{models.Stats{}, "", false},
		{models.Stats{Pending: 3, Held: 1, OldestPending: 4 * time.Second}, "3 events wait for their sinks (1 of them for their writers); the oldest has waited 4s.", true},
		{models.Stats{Pending: 120, Held: 120, OldestPending: 12 * time.Minute}, "120 events wait for their sinks (120 of them for their writers), the oldest for 12m0s: is a worker running the outbox drain?", false},
		{models.Stats{Dead: 2, LastDead: "webhook: hook answered 500"}, "2 events were given up on after 10 tries in the last 30 days; the newest failed with: webhook: hook answered 500", false},
	}
	noteType := fmt.Sprintf("%T", helpers.SystemCheckNote(""))
	for _, c := range cases {
		f.now = c.stats
		err := probe(context.Background())
		if c.want == "" {
			if err != nil {
				t.Errorf("%+v: %v, want healthy", c.stats, err)
			}
			continue
		}
		if err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v, want %q", c.stats, err, c.want)
		}
		if isNote := fmt.Sprintf("%T", err) == noteType; isNote != c.note {
			t.Errorf("%+v: a note (healthy, with words) = %v, want %v", c.stats, isNote, c.note)
		}
	}
}

func TestAnEventNeedsATopicAKindAndAKey(t *testing.T) {
	withFake(t)
	for _, e := range []Entry{{Kind: "k", Key: "k"}, {Topic: "t", Key: "k"}, {Topic: "t", Kind: "k"}} {
		if _, err := Add(context.Background(), nil, e); err == nil {
			t.Errorf("%+v was added", e)
		}
	}
	if _, err := Add(context.Background(), nil, Entry{Topic: "t", Kind: "k", Key: "k", Actor: uuid.NewString()}); err != nil {
		t.Error(err)
	}
}

func TestFirstTimeTellsWithoutRedis(t *testing.T) {
	if !FirstTime(context.Background(), Event{EventID: uuid.NewString()}, "notify", "u1") {
		t.Error("without Redis a sink should tell (a repeat beats a silence)")
	}
}

// Told lets through those a sink hasn't told yet; nil, everyone.
func TestToldLetsThroughOnlyThoseNotToldYet(t *testing.T) {
	recipients := []string{"u-sam", "u-jonas", "u-maya"}
	if got := strings.Join(Told(nil).Only("push", recipients), ","); got != "u-sam,u-jonas,u-maya" {
		t.Errorf("nil: %s", got)
	}
	toldBefore := map[string]bool{"push|u-jonas": true}
	tell := Told(func(how, r string) bool { return !toldBefore[how+"|"+r] })
	if got := strings.Join(tell.Only("push", recipients), ","); got != "u-sam,u-maya" {
		t.Errorf("push: %s", got)
	}
	if got := strings.Join(tell.Only("email", recipients), ","); got != "u-sam,u-jonas,u-maya" {
		t.Errorf("email is its own way: %s", got)
	}
}

// A writer releases an event with what it learnt while writing (a DM's first
// message: whom the new conversation is for), and its sinks are handed it.
func TestAnEventReleasedWithMoreCarriesItToItsSinks(t *testing.T) {
	f := withFake(t)
	var got map[string]string
	Sinks.Register(Sink{Topic: "test", Name: "access", Run: func(_ context.Context, ev Event) error {
		got = ev.Entities
		return nil
	}})
	id, err := Add(context.Background(), nil, Entry{Topic: "test", Kind: "test.made", Key: "dm:a b", Entities: map[string]string{"message": "m1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseWith(context.Background(), id, map[string]string{"opened": "a,b"}); err != nil {
		t.Fatal(err)
	}
	if f.stateOf(id) != models.StateReady {
		t.Fatalf("state %s: released", f.stateOf(id))
	}
	_ = Drain(context.Background())
	if got["message"] != "m1" || got["opened"] != "a,b" {
		t.Errorf("the sink was handed %v", got)
	}
}

// A change that makes many events (CreateTasks' new tasks, a table's rows)
// writes them in one statement and releases them in one, and its delivery
// claims them in one: the writer's path costs two statements however many
// there are.
func TestManyEventsCostOneStatementEachToWriteReleaseAndClaim(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "graph", 1, nil))
	ctx := context.Background()
	var es []Entry
	for _, m := range []string{"a", "b", "c"} {
		es = append(es, Entry{Topic: "test", Kind: "test.made", Key: "task:" + m, Entities: map[string]string{"message": m}})
	}
	ids, err := AddMany(ctx, nil, es)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] >= ids[1] || ids[1] >= ids[2] {
		t.Fatalf("ids %v: one each, in the order given", ids)
	}
	if err := ReleaseMany(ctx, ids); err != nil {
		t.Fatal(err)
	}
	DeliverMany(ids)
	helpers.SettleBackground(10 * time.Second)
	got := rec.calls()
	sort.Strings(got)
	if strings.Join(got, ",") != "graph#a,graph#b,graph#c" {
		t.Errorf("ran %v: each event once", got)
	}
	for _, id := range ids {
		if s := f.stateOf(id); s != models.StateDone {
			t.Errorf("event %d is %s, want done", id, s)
		}
	}
	if ins, rel, cl := f.statements(); ins != 1 || rel != 1 || cl != 1 {
		t.Errorf("%d inserts, %d releases, %d claims: one statement each", ins, rel, cl)
	}
}

// Every event of a batch is checked as Add checks one, before anything is
// written; an empty batch costs nothing.
func TestABatchWithABadEventWritesNothing(t *testing.T) {
	f := withFake(t)
	ctx := context.Background()
	if _, err := AddMany(ctx, nil, []Entry{{Topic: "t", Kind: "k", Key: "k"}, {Topic: "t", Kind: "k"}}); err == nil {
		t.Error("a batch holding an event with no key was added")
	}
	if ids, err := AddMany(ctx, nil, nil); err != nil || ids != nil {
		t.Errorf("an empty batch answered %v, %v", ids, err)
	}
	if err := ReleaseMany(ctx, nil); err != nil {
		t.Error(err)
	}
	DeliverMany(nil)
	helpers.SettleBackground(10 * time.Second)
	if ins, rel, cl := f.statements(); ins+rel+cl != 0 || len(f.rows) != 0 {
		t.Errorf("%d statements and %d rows for a bad or empty batch, want none", ins+rel+cl, len(f.rows))
	}
}

// A key's events delivered in one call still run one at a time, oldest
// first; another key's run beside them.
func TestEventsDeliveredTogetherKeepTheirKeysOrder(t *testing.T) {
	withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "realtime", 1, nil))
	m1 := add(t, "channel:c1", "m1")
	m2 := add(t, "channel:c1", "m2")
	o1 := add(t, "channel:c2", "o1")
	DeliverMany([]int64{m2, o1, m1})
	helpers.SettleBackground(10 * time.Second)
	got := rec.calls()
	var c1 []string
	for _, c := range got {
		if c != "realtime#o1" {
			c1 = append(c1, c)
		}
	}
	if len(got) != 3 || strings.Join(c1, ",") != "realtime#m1,realtime#m2" {
		t.Errorf("ran %v: c1's m1 then m2, and c2's o1", got)
	}
}

// A long list delivered at once is claimed a batch at a time, each batch under
// a lease of its own, so the last ones' leases don't run out while the first
// ones run: every event still runs once.
func TestALongListIsDeliveredABatchAtATime(t *testing.T) {
	f := withFake(t)
	rec := &recorder{}
	Sinks.Register(rec.sink("test", "graph", 1, nil))
	ctx := context.Background()
	es := make([]Entry, batch+1)
	for i := range es {
		m := strconv.Itoa(i)
		es[i] = Entry{Topic: "test", Kind: "test.made", Key: "row:" + m, Entities: map[string]string{"message": m}}
	}
	ids, err := AddMany(ctx, nil, es)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseMany(ctx, ids); err != nil {
		t.Fatal(err)
	}
	DeliverMany(ids)
	helpers.SettleBackground(10 * time.Second)
	if got := len(rec.calls()); got != len(ids) {
		t.Errorf("%d events ran, want each of the %d once", got, len(ids))
	}
	if _, _, cl := f.statements(); cl != 2 {
		t.Errorf("%d claims for %d events, want one per batch of %d", cl, len(ids), batch)
	}
}
