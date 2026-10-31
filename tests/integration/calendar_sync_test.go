//go:build integration
// +build integration

package integration_test

// Calendar sync's ledger (calendar B6) against real Postgres 12: the
// migrations up, down and up; a move started once while it runs, and again
// once it is stuck; and what is OneCamp's to send: an event with no ref, one
// changed after its ref, one whose instance changed, and never one just
// stamped in step. Then C9: a change the provider refused waits until its
// retry or until the event changes, a calendar can be marked read-only, a
// link breaks and is connected again, and a deactivated person's links wait.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarSync' -v

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/akashc777/OneCamp/tests/integration"
)

// migrationSQL is the one migration named name (its number aside), up or
// down.
func migrationSQL(t *testing.T, name, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_"+name+"."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one %s %s migration, found %v", name, dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarSyncMigrationUpDownUp(t *testing.T) {
	// calendar_sync_refusals, a later migration, points at calendar_sync_links:
	// the down takes it first and the up puts it back after, as migrating down
	// and up again would.
	up := migrationSQL(t, "calendar_sync", "up") + migrationSQL(t, "calendar_sync_refusals", "up")
	down := migrationSQL(t, "calendar_sync_refusals", "down") + migrationSQL(t, "calendar_sync", "down")
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
		for _, table := range []string{"calendar_sync_links", "calendar_sync_calendars", "calendar_sync_refs"} {
			if got := tableExists(t, env.PG, table); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, table, got)
			}
		}
	}
}

func TestCalendarSyncLedger(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
	// The models run on the server's pool (models/pgexec): this test's database.
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	person, calendar := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, person, person.String()+"@example.test")
	must(err)
	_, err = db.Exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, person)
	must(err)

	// A move starts once while it runs, and again once it has stopped.
	link, started, err := models.StartMove(ctx, uuid.New(), person, "google", "sam@outlook.com", time.Hour)
	if err != nil || !started || link.State != models.SyncMoving {
		t.Fatalf("first: %+v %v %v", link, started, err)
	}
	if _, again, err := models.StartMove(ctx, uuid.New(), person, "google", "sam@outlook.com", time.Hour); err != nil || again {
		t.Fatalf("a second move while one runs: %v %v", again, err)
	}
	skipped, _ := json.Marshal([]map[string]string{{"what": "Broken", "why": "It couldn't be read from Google."}})
	must(models.FinishMove(ctx, link.ID, models.SyncSynced, 12, 3, skipped, ""))
	read, err := models.SyncLinkOf(ctx, person, "google")
	if err != nil || read.State != models.SyncSynced || read.Moved != 12 || read.Unchanged != 3 || read.Progress != 100 || read.LastSyncedAt == nil {
		t.Fatalf("finished: %+v %v", read, err)
	}
	links, err := models.SyncedLinks(ctx, "google", 10)
	if err != nil || len(links) != 1 {
		t.Fatalf("synced links %v %v", links, err)
	}
	must(models.KeepSyncCalendar(ctx, models.SyncCalendar{LinkID: link.ID, RemoteID: "sam@outlook.com", Name: "sam@outlook.com", Primary: true, CalendarID: &calendar, Cursor: "tok-1"}))
	cals, err := models.SyncCalendarsOf(ctx, link.ID)
	if err != nil || len(cals) != 1 || cals[0].Cursor != "tok-1" || *cals[0].CalendarID != calendar {
		t.Fatalf("calendars %+v %v", cals, err)
	}

	// Three events: one sent and in step, one changed since, one never sent.
	event := func() uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour', NOW())`,
			id, calendar, person, id.String()+"@onecamp")
		must(err)
		return id
	}
	inStep, changed, fresh := event(), event(), event()
	must(models.KeepSyncRefs(ctx, []models.SyncRef{
		{LinkID: link.ID, RemoteID: "sam@outlook.com", ExternalID: "g1", EventID: inStep, ETag: "e1"},
		{LinkID: link.ID, RemoteID: "sam@outlook.com", ExternalID: "g2", EventID: changed, ETag: "e2"},
	}))
	_, err = db.Exec(`UPDATE events SET title = 'Changed', updated_at = NOW() + interval '1 minute' WHERE id = $1`, changed)
	must(err)
	dirty, err := models.ChangedSince(ctx, db, link.ID, person, []uuid.UUID{calendar}, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, e := range dirty {
		got[e.ID] = true
	}
	if len(got) != 2 || !got[changed] || !got[fresh] || got[inStep] {
		t.Fatalf("to send: %v (changed %s, fresh %s, in step %s)", got, changed, fresh, inStep)
	}
	// Stamped again, the changed one is in step: no earlier than its change.
	must(models.KeepSyncRefs(ctx, []models.SyncRef{{LinkID: link.ID, RemoteID: "sam@outlook.com", ExternalID: "g2", EventID: changed, ETag: "e3"}}))
	refs, err := models.SyncRefsOf(ctx, link.ID)
	if err != nil || refs[changed].ETag != "e3" {
		t.Fatalf("refs %+v %v", refs, err)
	}
	dirty, _ = models.ChangedSince(ctx, db, link.ID, person, []uuid.UUID{calendar}, time.Now(), 10)
	if len(dirty) != 1 || dirty[0].ID != fresh {
		t.Fatalf("after the stamp: %+v", dirty)
	}
	must(models.DropSyncRefs(ctx, link.ID, []uuid.UUID{inStep}))
	if refs, _ := models.SyncRefsOf(ctx, link.ID); len(refs) != 1 {
		t.Fatalf("after the drop: %+v", refs)
	}
}

func TestCalendarSyncRefusalsMigrationUpDownUp(t *testing.T) {
	up, down := migrationSQL(t, "calendar_sync_refusals", "up"), migrationSQL(t, "calendar_sync_refusals", "down")
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
		var broken bool
		if err := env.PG.QueryRow(`SELECT pg_get_constraintdef(oid) LIKE '%broken%' FROM pg_constraint WHERE conname = 'calendar_sync_links_state_check'`).Scan(&broken); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := [3]bool{tableExists(t, env.PG, "calendar_sync_refusals"), columnExists(t, env, "calendar_sync_calendars", "read_only"), broken}; got != [3]bool{step.want, step.want, step.want} {
			t.Fatalf("step %d: refusals, read_only, broken = %v, want all %v", i, got, step.want)
		}
	}
}

// syncLedgerOf is a person with a calendar and a synced Google link, made
// in the test's database, which the models run on.
func syncLedgerOf(t *testing.T, ctx context.Context, env *integration.Env) (person, calendar uuid.UUID, link *models.SyncLink) {
	t.Helper()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	person, calendar = uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, person, person.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, person); err != nil {
		t.Fatal(err)
	}
	link, _, err := models.StartMove(ctx, uuid.New(), person, "google", "sam@outlook.com", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	return person, calendar, link
}

// A deactivated person's links aren't synced, so they don't hold the first
// places of every sweep either.
func TestCalendarSyncLeavesADeactivatedPersonsLinks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	person, _, link := syncLedgerOf(t, ctx, env)
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, person); err != nil {
		t.Fatal(err)
	}
	links, err := models.SyncedLinks(ctx, "google", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.ID == link.ID {
			t.Fatal("a deactivated person's link is still synced")
		}
	}
}

// C9: a change the provider refused waits until its retry, or, refused for
// good, until the event changes; one that failed in step (an instance) is
// sent again at its retry; a ref kept or dropped ends the refusal. A calendar
// can be marked read-only, and a link breaks and is connected again, by its
// own Google account only.
func TestCalendarSyncRefusalsWaitAndLinksBreak(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
	person, calendar, link := syncLedgerOf(t, ctx, env)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	toSend := func(at time.Time) map[uuid.UUID]bool {
		t.Helper()
		dirty, err := models.ChangedSince(ctx, db, link.ID, person, []uuid.UUID{calendar}, at, 10)
		must(err)
		got := map[uuid.UUID]bool{}
		for _, e := range dirty {
			got[e.ID] = true
		}
		return got
	}
	event := func() uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour', NOW())`,
			id, calendar, person, id.String()+"@onecamp")
		must(err)
		return id
	}
	later, retry := now.Add(time.Hour), now.Add(30*time.Minute)

	// A new event that failed for now waits for its retry.
	waiting := event()
	must(models.KeepSyncRefusal(ctx, models.SyncRefusal{LinkID: link.ID, EventID: waiting, Tries: 1, RetryAt: &retry, Why: "503"}, now))
	if toSend(now)[waiting] || !toSend(later)[waiting] {
		t.Fatalf("a failed change: sent now %v, after its retry %v", toSend(now)[waiting], toSend(later)[waiting])
	}
	// Refused for good: it waits past any retry, until it changes.
	refused := event()
	must(models.KeepSyncRefusal(ctx, models.SyncRefusal{LinkID: link.ID, EventID: refused, Tries: 3, Why: "400"}, now))
	if toSend(later)[refused] {
		t.Fatal("a change refused for good is sent again unchanged")
	}
	_, err := db.Exec(`UPDATE events SET title = 'Changed', updated_at = $2 WHERE id = $1`, refused, now.Add(time.Minute))
	must(err)
	if !toSend(later)[refused] {
		t.Fatal("a refused event isn't sent once it changes")
	}
	got, err := models.SyncRefusalsOf(ctx, link.ID, []uuid.UUID{waiting, refused})
	if err != nil || got[waiting].Tries != 1 || got[waiting].RetryAt == nil || got[refused].Tries != 3 || got[refused].RetryAt != nil {
		t.Fatalf("refusals %+v %v", got, err)
	}
	// In step, but an instance of it failed: sent again at its retry.
	inStep := event()
	must(models.KeepSyncRefs(ctx, []models.SyncRef{{LinkID: link.ID, RemoteID: "sam@outlook.com", ExternalID: "g1", EventID: inStep}}))
	must(models.KeepSyncRefusal(ctx, models.SyncRefusal{LinkID: link.ID, EventID: inStep, Tries: 1, RetryAt: &retry, Why: "503"}, now))
	if toSend(now)[inStep] || !toSend(later)[inStep] {
		t.Fatal("an event whose instance failed isn't sent at its retry, or is sent before")
	}
	// Kept in step, or dropped, the refusal is over.
	must(models.KeepSyncRefs(ctx, []models.SyncRef{{LinkID: link.ID, RemoteID: "sam@outlook.com", ExternalID: "g1", EventID: inStep, ETag: "e2"}}))
	must(models.DropSyncRefs(ctx, link.ID, []uuid.UUID{waiting}))
	if got, err := models.SyncRefusalsOf(ctx, link.ID, []uuid.UUID{waiting, refused, inStep}); err != nil || len(got) != 1 || got[refused].Tries != 3 {
		t.Fatalf("after keeping and dropping: %+v %v", got, err)
	}

	// A calendar found read-only stays so through the move's next write.
	must(models.KeepSyncCalendar(ctx, models.SyncCalendar{LinkID: link.ID, RemoteID: "team@group", Name: "Team", CalendarID: &calendar}))
	must(models.MarkSyncCalendarReadOnly(ctx, link.ID, "team@group"))
	cals, err := models.SyncCalendarsOf(ctx, link.ID)
	if err != nil || len(cals) != 1 || !cals[0].ReadOnly {
		t.Fatalf("calendars %+v %v", cals, err)
	}
	must(models.KeepSyncCalendar(ctx, cals[0]))
	if cals, _ := models.SyncCalendarsOf(ctx, link.ID); !cals[0].ReadOnly {
		t.Fatal("keeping the calendar again forgot it's read-only")
	}

	// Broken, the link isn't synced; connected again, it is.
	synced := func() bool {
		t.Helper()
		links, err := models.SyncedLinks(ctx, "google", 10)
		must(err)
		return len(links) == 1 && links[0].ID == link.ID
	}
	must(models.BreakSyncLink(ctx, link.ID, "Google stopped answering for this account. Connect it again."))
	if l, _ := models.SyncLinkOf(ctx, person, "google"); l.State != models.SyncBroken || l.Error == "" || synced() {
		t.Fatalf("broken: %+v, synced %v", l, synced())
	}
	// Another Google account connected, or one whose address Google didn't
	// give, leaves it broken: that token isn't this link's account's.
	for _, other := range []string{"lee@outlook.com", ""} {
		must(models.ReconnectSyncLinks(ctx, person, "google", other))
		if l, _ := models.SyncLinkOf(ctx, person, "google"); l.State != models.SyncBroken || l.Account != "sam@outlook.com" || synced() {
			t.Fatalf("%q connected: %+v, synced %v", other, l, synced())
		}
	}
	// Its own account, however Google writes the address, syncs it again.
	must(models.ReconnectSyncLinks(ctx, person, "google", "Sam@Outlook.com"))
	if l, _ := models.SyncLinkOf(ctx, person, "google"); l.State != models.SyncSynced || l.Error != "" || l.Account != "sam@outlook.com" || !synced() {
		t.Fatalf("connected again: %+v, synced %v", l, synced())
	}
	// A link kept before its address was takes the account connected.
	_, err = db.Exec(`UPDATE calendar_sync_links SET account = '' WHERE id = $1`, link.ID)
	must(err)
	must(models.BreakSyncLink(ctx, link.ID, "Google stopped answering for this account. Connect it again."))
	must(models.ReconnectSyncLinks(ctx, person, "google", "sam@outlook.com"))
	if l, _ := models.SyncLinkOf(ctx, person, "google"); l.State != models.SyncSynced || l.Account != "sam@outlook.com" || !synced() {
		t.Fatalf("a link without its address, connected again: %+v, synced %v", l, synced())
	}
}
