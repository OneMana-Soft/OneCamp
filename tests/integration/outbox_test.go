//go:build integration
// +build integration

package integration_test

// The outbox (business/Outbox, chat A2) against real Postgres 12 and Redis:
// its migration goes up, down and up again; an event written in a transaction
// that rolls back never runs; two drains at once give each event to each sink
// once, a key's events in order, and the writers' own deliveries keep that
// order too; a sink that fails is tried again alone; a held
// event is decided by its topic once its writer has had its time; the admins'
// check sees a stuck drain and the dead letters; FirstTime remembers a
// delivery past the retry horizon; and the purge keeps dead events longer.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOutbox|TestTheOutbox|TestFirstTime' -v

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	outboxModels "github.com/akashc777/OneCamp/models/postgres/Outbox"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/tests/integration"
)

// TestOutboxMigrationUpDownUp: the migration goes up, down and up again on
// Postgres 12, numbered as the chat programme's are.
func TestOutboxMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	read := func(direction string) (string, string) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*_outbox."+direction+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one outbox %s migration, found %v", direction, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Base(matches[0]), string(b)
	}
	upName, up := read("up")
	_, down := read("down")
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n == 198 || n == 199 || n < 240 {
		t.Fatalf("%s: a chat programme migration is numbered from 240, and never 198 or 199", upName)
	}
	if strings.Contains(up, "gen_random_uuid") || strings.Contains(up, "uuid_generate") {
		t.Fatalf("%s makes uuids in the database; Postgres 12 has neither function by default", upName)
	}
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if tableExists(t, env.PG, "outbox") != step.want {
			t.Fatalf("step %d: outbox exists = %v", i, !step.want)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO outbox (event_id, topic, kind, key, state) VALUES ($1, 't', 'k', 'x', 'lost')`, uuid.New()); err == nil {
		t.Fatal("a row in no state was kept")
	}
}

// outboxWorld is Postgres with the project's pool on it, and Redis.
func outboxWorld(t *testing.T) *integration.Env {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	return env
}

// testTopic is a topic of the test's own, so its sinks meet no one else's.
func testTopic() string { return "test.outbox." + uuid.NewString()[:8] }

// addReleased writes an event in its own transaction and releases it.
func addReleased(t *testing.T, e outboxBusiness.Entry) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := outboxBusiness.Add(ctx, tx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := outboxBusiness.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func stateOf(t *testing.T, env *integration.Env, id int64) (state string, done []string) {
	t.Helper()
	var sinks string
	if err := env.PG.QueryRow(`SELECT state, array_to_string(sinks_done, ',') FROM outbox WHERE id = $1`, id).Scan(&state, &sinks); err != nil {
		t.Fatal(err)
	}
	if sinks != "" {
		done = strings.Split(sinks, ",")
	}
	return state, done
}

func TestTheOutboxGivesEachEventToEachSinkOnceAcrossTwoDrains(t *testing.T) {
	env := outboxWorld(t)
	ctx := context.Background()
	topic := testTopic()
	var mu sync.Mutex
	runs := map[string]int{}
	order := map[string][]int{}
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: topic, Name: "count", Run: func(_ context.Context, ev outboxBusiness.Event) error {
		n, _ := strconv.Atoi(ev.Entities["n"])
		mu.Lock()
		runs[ev.Entities["n"]]++
		order[ev.Key] = append(order[ev.Key], n)
		mu.Unlock()
		return nil
	}})

	for i := 0; i < 40; i++ {
		addReleased(t, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: fmt.Sprintf("k%d", i%4), Entities: map[string]string{"n": strconv.Itoa(i)}})
	}
	// An event written in a transaction that rolls back was never there.
	tx, _ := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if _, err := outboxBusiness.Add(ctx, tx, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: "k0", Entities: map[string]string{"n": "rolled-back"}}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()

	var wg sync.WaitGroup
	for d := 0; d < 2; d++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := outboxBusiness.Drain(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	_ = outboxBusiness.Drain(ctx)
	helpers.SettleBackground(10 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 40; i++ {
		if n := runs[strconv.Itoa(i)]; n != 1 {
			t.Errorf("event %d ran %d times, want once", i, n)
		}
	}
	if runs["rolled-back"] != 0 {
		t.Error("an event whose transaction rolled back ran")
	}
	for key, ns := range order {
		for i := 1; i < len(ns); i++ {
			if ns[i] < ns[i-1] {
				t.Errorf("%s ran %v: a key's events go oldest first", key, ns)
				break
			}
		}
	}
	var left int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM outbox WHERE topic = $1 AND state <> 'done'`, topic).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows not done (%v)", left, err)
	}
}

// TestTheOutboxDeliversAKeysEventsOldestFirstWhoeverDeliversFirst: two
// messages' writers deliver their events themselves, the later one first; it
// takes nothing while the earlier one is due, and the earlier one's delivery
// runs both, in order.
func TestTheOutboxDeliversAKeysEventsOldestFirstWhoeverDeliversFirst(t *testing.T) {
	outboxWorld(t)
	topic := testTopic()
	var mu sync.Mutex
	var ran []string
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: topic, Name: "realtime", Run: func(_ context.Context, ev outboxBusiness.Event) error {
		mu.Lock()
		ran = append(ran, ev.Entities["n"])
		mu.Unlock()
		return nil
	}})
	first := addReleased(t, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: "channel:c1", Entities: map[string]string{"n": "1"}})
	second := addReleased(t, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: "channel:c1", Entities: map[string]string{"n": "2"}})
	outboxBusiness.Deliver(second)
	helpers.SettleBackground(10 * time.Second)
	mu.Lock()
	early := strings.Join(ran, ",")
	mu.Unlock()
	if early != "" {
		t.Fatalf("ran %q while the key's earlier event waited", early)
	}
	outboxBusiness.Deliver(first)
	helpers.SettleBackground(10 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(ran, ","); got != "1,2" {
		t.Errorf("ran %q: the earlier event, then the later", got)
	}
}

func TestTheOutboxTriesAFailedSinkAgainAloneAndGivesUpVisibly(t *testing.T) {
	env := outboxWorld(t)
	ctx := context.Background()
	topic := testTopic()
	var mu sync.Mutex
	steady, flaky := 0, 0
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: topic, Name: "steady", Order: 1, Run: func(context.Context, outboxBusiness.Event) error {
		mu.Lock()
		steady++
		mu.Unlock()
		return nil
	}})
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: topic, Name: "flaky", Order: 2, Run: func(context.Context, outboxBusiness.Event) error {
		mu.Lock()
		defer mu.Unlock()
		flaky++
		if flaky == 1 {
			return fmt.Errorf("the push service answered 503")
		}
		return nil
	}})
	id := addReleased(t, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: "k", Entities: map[string]string{"n": "1"}})
	_ = outboxBusiness.Drain(ctx)
	if state, done := stateOf(t, env, id); state != outboxModels.StateReady || strings.Join(done, ",") != "steady" {
		t.Fatalf("after a failure: %s %v, want ready with steady done", state, done)
	}
	var lastErr string
	_ = env.PG.QueryRow(`SELECT last_error FROM outbox WHERE id = $1`, id).Scan(&lastErr)
	if !strings.Contains(lastErr, "flaky: the push service answered 503") {
		t.Errorf("last error %q", lastErr)
	}
	// Due again (the backoff, run out).
	if _, err := env.PG.Exec(`UPDATE outbox SET available_at = NOW() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_ = outboxBusiness.Drain(ctx)
	if state, _ := stateOf(t, env, id); state != outboxModels.StateDone || steady != 1 || flaky != 2 {
		t.Errorf("state %s, steady %d, flaky %d: the retry gives the event to the failed sink only", state, steady, flaky)
	}

	// One that never succeeds is given up on after its last try, and kept.
	never := testTopic()
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: never, Name: "webhook", Run: func(context.Context, outboxBusiness.Event) error {
		return fmt.Errorf("the hook answered 500")
	}})
	dead := addReleased(t, outboxBusiness.Entry{Topic: never, Kind: "test.made", Key: "k2"})
	for i := 0; i < outboxBusiness.MaxAttempts; i++ {
		if _, err := env.PG.Exec(`UPDATE outbox SET available_at = NOW() WHERE id = $1 AND state = 'ready'`, dead); err != nil {
			t.Fatal(err)
		}
		_ = outboxBusiness.Drain(ctx)
	}
	var attempts int
	var finished *time.Time
	if err := env.PG.QueryRow(`SELECT attempts, finished_at FROM outbox WHERE id = $1`, dead).Scan(&attempts, &finished); err != nil {
		t.Fatal(err)
	}
	if state, _ := stateOf(t, env, dead); state != outboxModels.StateDead || attempts != outboxBusiness.MaxAttempts || finished == nil {
		t.Errorf("state %s after %d tries: dead, with when", state, attempts)
	}

	// The admins' check says so, and says when the drain is stuck.
	check := func() string {
		for _, r := range helpers.RunSystemChecks(ctx, 5*time.Second) {
			if r.Name == "outbox" {
				if r.Healthy {
					return "healthy: " + r.Detail
				}
				return r.Detail
			}
		}
		return "no outbox check"
	}
	if got := check(); !strings.Contains(got, "given up on after 10 tries") || !strings.Contains(got, "the hook answered 500") {
		t.Errorf("check: %q", got)
	}
	stuck := addReleased(t, outboxBusiness.Entry{Topic: testTopic(), Kind: "test.made", Key: "k3"})
	if _, err := env.PG.Exec(`UPDATE outbox SET created_at = NOW() - interval '12 minutes' WHERE id = $1`, stuck); err != nil {
		t.Fatal(err)
	}
	if got := check(); !strings.Contains(got, "is a worker running the outbox drain?") {
		t.Errorf("check with a stuck event: %q", got)
	}
}

func TestTheOutboxDecidesAHeldEventByItsTopicOnceItsWriterHadItsTime(t *testing.T) {
	env := outboxWorld(t)
	ctx := context.Background()
	topic := testTopic()
	var mu sync.Mutex
	var ran []string
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: topic, Name: "count", Run: func(_ context.Context, ev outboxBusiness.Event) error {
		mu.Lock()
		ran = append(ran, ev.Entities["n"])
		mu.Unlock()
		return nil
	}})
	outboxBusiness.Sinks.Resolve(topic, func(_ context.Context, ev outboxBusiness.Event) (bool, error) {
		return ev.Entities["n"] == "written", nil
	})
	held := func(n string) int64 {
		tx, _ := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
		id, err := outboxBusiness.Add(ctx, tx, outboxBusiness.Entry{Topic: topic, Kind: "test.made", Key: "k-" + n, Entities: map[string]string{"n": n}})
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit()
		return id
	}
	written, lost := held("written"), held("lost")
	// Within its writer's time, nothing is decided.
	_ = outboxBusiness.Drain(ctx)
	if state, _ := stateOf(t, env, written); state != outboxModels.StateHeld {
		t.Fatalf("decided after %s: %s", "no time", state)
	}
	if _, err := env.PG.Exec(`UPDATE outbox SET created_at = NOW() - interval '1 minute' WHERE topic = $1`, topic); err != nil {
		t.Fatal(err)
	}
	_ = outboxBusiness.Drain(ctx)
	helpers.SettleBackground(10 * time.Second)
	if state, _ := stateOf(t, env, written); state != outboxModels.StateDone {
		t.Errorf("a written change's event: %s, want released and done", state)
	}
	if state, _ := stateOf(t, env, lost); state != outboxModels.StateCancelled {
		t.Errorf("a lost change's event: %s, want cancelled", state)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ran, ",") != "written" {
		t.Errorf("ran %v: only the written change's event reaches its sinks", ran)
	}
}

func TestFirstTimeRemembersADeliveryPastTheRetryHorizon(t *testing.T) {
	outboxWorld(t)
	ctx := context.Background()
	ev := outboxBusiness.Event{EventID: uuid.NewString()}
	if !outboxBusiness.FirstTime(ctx, ev, "post.notify:push", "u-sam") {
		t.Fatal("the first delivery wasn't the first")
	}
	if outboxBusiness.FirstTime(ctx, ev, "post.notify:push", "u-sam") {
		t.Error("a retry would tell the same person again")
	}
	if !outboxBusiness.FirstTime(ctx, ev, "post.notify:push", "u-jonas") || !outboxBusiness.FirstTime(ctx, ev, "post.notify:email", "u-sam") {
		t.Error("the claim is per (event, sink, recipient)")
	}
	ttl, err := redisInit.RedisClient.TTL(ctx, registry.OutboxOnce.Build(ev.EventID, "post.notify:push", "u-sam")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= outboxBusiness.RetryHorizon() {
		t.Errorf("the claim lasts %s, no longer than the retry horizon %s: a late retry would tell them twice", ttl, outboxBusiness.RetryHorizon())
	}
}

func TestTheOutboxPurgeKeepsDeadEventsLongerThanDoneOnes(t *testing.T) {
	env := outboxWorld(t)
	ctx := context.Background()
	insert := func(state string, age time.Duration) int64 {
		var id int64
		if err := env.PG.QueryRow(`INSERT INTO outbox (event_id, topic, kind, key, state, finished_at)
			VALUES ($1, 'test.purge', 'test.made', 'k', $2, NOW() - make_interval(secs => $3)) RETURNING id`,
			uuid.New(), state, age.Seconds()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	day := 24 * time.Hour
	oldDone, recentDone, oldDead, recentDead, oldCancelled := insert("done", 8*day), insert("done", 6*day), insert("dead", 31*day), insert("dead", 8*day), insert("cancelled", 8*day)
	now := time.Now()
	if _, err := outboxModels.Purge(ctx, now.Add(-7*day), now.Add(-30*day), 1000); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]bool{oldDone: false, recentDone: true, oldDead: false, recentDead: true, oldCancelled: false} {
		var n int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id = $1`, id).Scan(&n)
		if (n == 1) != want {
			t.Errorf("row %d kept = %v, want %v", id, n == 1, want)
		}
	}
}
