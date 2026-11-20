//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// googleAtTheOutbox stands in for Google for the sync sink and the sweep: it
// counts what each event's title is sent, answers by the title, and holds a
// send a while, so a second sender has time to try.
type googleAtTheOutbox struct {
	mu    sync.Mutex
	sent  map[string]int
	holds time.Duration
}

var outboxGoogle = &googleAtTheOutbox{sent: map[string]int{}, holds: 400 * time.Millisecond}

func (*googleAtTheOutbox) Provider() string { return "google" }

func (*googleAtTheOutbox) Calendars(context.Context, calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	return []calendarSyncBusiness.Remote{{ID: "primary", Name: "Olu", Primary: true}}, nil
}

func (*googleAtTheOutbox) Pull(_ context.Context, _ calendarSyncBusiness.Remote, _ string) ([]calendarSyncBusiness.Change, string, error) {
	return nil, "c1", nil
}

func (g *googleAtTheOutbox) Push(_ context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	g.mu.Lock()
	g.sent[op.Event.Summary]++
	n := g.sent[op.Event.Summary]
	g.mu.Unlock()
	time.Sleep(g.holds)
	switch op.Event.Summary {
	case "Refused at Google":
		return calendarSyncBusiness.Ref{}, fmt.Errorf("%w: 400 invalid", calendarSyncBusiness.ErrRefused)
	case "Flaky at Google":
		return calendarSyncBusiness.Ref{}, errors.New("google: 503 backend error")
	case "Revoked at Google":
		return calendarSyncBusiness.Ref{}, calendarSyncBusiness.ErrRevoked
	}
	return calendarSyncBusiness.Ref{ExternalID: "g-" + op.EventUUID.String(), ETag: fmt.Sprint(n)}, nil
}

func (*googleAtTheOutbox) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, nil
}

func (*googleAtTheOutbox) Stop(context.Context, calendarSyncBusiness.Watch) error { return nil }

func (g *googleAtTheOutbox) sentOf(title string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sent[title]
}

// A change to an event asks for its Google send, never when it rolls back, and
// that send is made once, whoever delivers it and however often: two drains
// and the sweep at the same time, a second delivery, the sweep after. A
// refusal is kept and a revoked account breaks the link, and both are done;
// a passing failure waits for the outbox's next try.
func TestCalendarWritesGoToGoogleOnceThroughTheOutbox(t *testing.T) {
	w := newRefsWorld(t)
	integration.MarkCalendarMoved(t, w.env.PG)
	t.Cleanup(calendarBusiness.SyncThroughForTest(outboxGoogle))
	ctx := context.Background()
	u := w.as("olu").Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	olu := &u
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	create := func(title string) (*calendarBusiness.Event, error) {
		start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
		end := start.Add(time.Hour)
		return calendarBusiness.Events.Create(w.as("olu"), olu, calendarBusiness.EventInput{Title: title,
			Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}})
	}
	type row struct {
		id       int64
		state    string
		attempts int
		free     bool
	}
	rowOf := func(event string) (r row) {
		t.Helper()
		must(w.env.PG.QueryRow(`SELECT id, state, attempts, claimed_until IS NULL OR claimed_until < NOW() FROM outbox
			WHERE topic = 'calendar.sync' AND key = $1 ORDER BY id DESC LIMIT 1`, "sync:"+event).Scan(&r.id, &r.state, &r.attempts, &r.free))
		return r
	}
	settled := func(event string, done func(row) bool) row {
		t.Helper()
		for until := time.Now().Add(20 * time.Second); ; {
			r := rowOf(event)
			if done(r) {
				return r
			}
			if time.Now().After(until) {
				t.Fatalf("the sync entry of %s never settled: %+v", event, r)
			}
			go func() { _ = outboxBusiness.Drain(ctx) }()
			time.Sleep(100 * time.Millisecond)
		}
	}
	isDone := func(r row) bool { return r.state == "done" }

	// The link, synced, and its primary calendar mapped to olu's own.
	link, _, err := models.StartMove(ctx, uuid.New(), w.people["olu"], "google", "olu@example.com", time.Hour)
	must(err)
	must(models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""))
	must(calendarBusiness.SyncLinkForTest(ctx, link, outboxGoogle)) // the sweep sees the link: olu's changes ask for sync entries (R384-N1)
	first, err := create("Before the link's calendar is mapped")
	must(err)
	settled(first.ID, isDone)
	calendar := uuid.MustParse(first.CalendarID)
	must(models.KeepSyncCalendar(ctx, models.SyncCalendar{LinkID: link.ID, RemoteID: "primary", Name: "Olu", CalendarID: &calendar, Cursor: "read whole"}))

	// A write that rolls back leaves no sync entry.
	var before, after int
	must(w.env.PG.QueryRow(`SELECT count(*) FROM outbox WHERE topic = 'calendar.sync'`).Scan(&before))
	_, err = w.env.PG.Exec(`CREATE OR REPLACE FUNCTION g18_refuse_event() RETURNS trigger AS $$
		BEGIN IF NEW.title = 'Rolled back' THEN RAISE EXCEPTION 'rolled back by the test'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER g18_refuse_event AFTER INSERT ON events DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE PROCEDURE g18_refuse_event()`)
	must(err)
	t.Cleanup(func() { _, _ = w.env.PG.Exec(`DROP TRIGGER IF EXISTS g18_refuse_event ON events`) })
	if _, err := create("Rolled back"); err == nil {
		t.Fatal("a write the database refused at commit was taken")
	}
	_, err = w.env.PG.Exec(`DROP TRIGGER g18_refuse_event ON events`)
	must(err)
	must(w.env.PG.QueryRow(`SELECT count(*) FROM outbox WHERE topic = 'calendar.sync'`).Scan(&after))
	if after != before {
		t.Fatalf("a rolled-back write left %d sync entries", after-before)
	}

	// One send, while two drains and the sweep try it too: the outbox's
	// claim holds the entry to one (one attempt), and the sweep waits for the
	// link the sink is sending.
	raced, err := create("Raced")
	must(err)
	for until := time.Now().Add(20 * time.Second); outboxGoogle.sentOf("Raced") == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(until) {
			t.Fatal("the sync entry was never delivered")
		}
		go func() { _ = outboxBusiness.Drain(ctx) }()
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = outboxBusiness.Drain(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); must(calendarBusiness.SyncLinkForTest(ctx, link, outboxGoogle)) }()
	wg.Wait()
	r := settled(raced.ID, isDone)
	if n := outboxGoogle.sentOf("Raced"); n != 1 || r.attempts != 1 {
		t.Fatalf("raced: sent %d times in %d attempts; want once in one", n, r.attempts)
	}
	// A second delivery, then the sweep, send nothing more.
	outboxBusiness.DeliverMany([]int64{r.id})
	must(outboxBusiness.Drain(ctx))
	must(calendarBusiness.SyncLinkForTest(ctx, link, outboxGoogle))
	time.Sleep(time.Second)
	if n := outboxGoogle.sentOf("Raced"); n != 1 {
		t.Fatalf("raced: sent %d times after a second delivery and the sweep; want once", n)
	}

	// A refusal is kept, and the entry is done.
	refused, err := create("Refused at Google")
	must(err)
	settled(refused.ID, isDone)
	kept, err := models.SyncRefusalsOf(ctx, link.ID, []uuid.UUID{uuid.MustParse(refused.ID)})
	must(err)
	if len(kept) != 1 {
		t.Fatalf("the refusal wasn't kept: %v", kept)
	}
	// M14: another person's event on a calendar the link maps isn't olu's to
	// send: the link's sweep and its sink leave it (the created_by test).
	theirs := uuid.New()
	_, err = w.env.PG.Exec(`INSERT INTO events SELECT (jsonb_populate_record(NULL::events, to_jsonb(e) || jsonb_build_object(
		'id', $2::uuid, 'created_by', $3::uuid, 'title', 'Mia''s, on olu''s calendar', 'ical_uid', $2::text, 'google_event_id', NULL))).*
		FROM events e WHERE e.id = $1`, raced.ID, theirs, w.people["mia"])
	must(err)
	must(calendarBusiness.SyncLinkForTest(ctx, link, outboxGoogle))
	after14, err := create("Olu's, after Mia's")
	must(err)
	settled(after14.ID, isDone)
	if n := outboxGoogle.sentOf("Mia's, on olu's calendar"); n != 0 {
		t.Fatalf("the link sent another person's event %d times", n)
	}
	// A passing failure waits for the next try: ready, one attempt more.
	flaky, err := create("Flaky at Google")
	must(err)
	if r := settled(flaky.ID, func(r row) bool { return r.attempts >= 1 && r.free }); r.state != "ready" || r.attempts != 1 {
		t.Fatalf("a passing failure: %+v; want ready after one attempt", r)
	}
	// A revoked account breaks the link, and the entry is done.
	revoked, err := create("Revoked at Google")
	must(err)
	settled(revoked.ID, isDone)
	now, err := models.SyncLinkOf(ctx, w.people["olu"], "google")
	must(err)
	if now == nil || now.State != models.SyncBroken {
		t.Fatalf("a revoked account left the link %+v", now)
	}
}
