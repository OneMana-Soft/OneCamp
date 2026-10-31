//go:build integration
// +build integration

package integration_test

// Calendars read from an address (calendar B8) against real Postgres 12: the
// migration up, down and up; a subscription added once per address, due,
// read by one reader at a time, and dropped; its calendar deleted with
// everything on it, and only an address's own events deleted when they
// leave its file.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestCalendarSubscriptionsMigrationUpDownUp|TestCalendarSubscriptions)$' -v

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/akashc777/OneCamp/tests/integration"
)

func subscriptionsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_subscriptions."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_subscriptions %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarSubscriptionsMigrationUpDownUp(t *testing.T) {
	up, down := subscriptionsMigration(t, "up"), subscriptionsMigration(t, "down")
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
		for _, col := range []string{"source_sealed", "read_at", "error"} {
			if got := columnExists(t, env, "calendar_sync_calendars", col); got != step.want {
				t.Fatalf("step %d: calendar_sync_calendars.%s exists = %v", i, col, got)
			}
		}
		if got := tableExists(t, env.PG, "calendar_sync_calendars_due_idx"); got != step.want {
			t.Fatalf("step %d: the due index exists = %v", i, got)
		}
	}
}

func TestCalendarSubscriptions(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
	// The subscription models run on the server's pool (models/pgexec): this test's database.
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	person := func(name string) uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name)
		must(err)
		return id
	}
	sam, maya := person("sam"), person("maya")

	link, err := models.EnsureSyncLink(ctx, uuid.New(), sam, "ics")
	if err != nil || link == nil || link.State != models.SyncSynced {
		t.Fatalf("link %+v %v", link, err)
	}
	again, err := models.EnsureSyncLink(ctx, uuid.New(), sam, "ics")
	if err != nil || again.ID != link.ID {
		t.Fatalf("a second link: %+v %v", again, err)
	}

	cal := uuid.New()
	must(models.SubscribedCalendar(ctx, cal, sam, "Team", "lake", "UTC"))
	sub := models.Subscription{SyncCalendar: models.SyncCalendar{LinkID: link.ID, RemoteID: "digest-1", Name: "Team", CalendarID: &cal}, PersonID: sam, SourceSealed: "sealed"}
	if added, err := models.AddSubscription(ctx, sub); err != nil || !added {
		t.Fatalf("added %v %v", added, err)
	}
	if added, err := models.AddSubscription(ctx, sub); err != nil || added {
		t.Fatalf("the same address twice: %v %v", added, err)
	}
	subs, err := models.SubscriptionsOf(ctx, sam)
	if err != nil || len(subs) != 1 || subs[0].SourceSealed != "sealed" || *subs[0].CalendarID != cal || subs[0].PersonID != sam {
		t.Fatalf("subscriptions %+v %v", subs, err)
	}

	// Due until read; one reader at a time.
	now := time.Now().UTC().Truncate(time.Second)
	due, err := models.DueSubscriptions(ctx, now.Add(-15*time.Minute), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due %+v %v", due, err)
	}
	if ok, err := models.ClaimSubscription(ctx, link.ID, "digest-1", now, now.Add(-15*time.Minute)); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if ok, err := models.ClaimSubscription(ctx, link.ID, "digest-1", now, now.Add(-15*time.Minute)); err != nil || ok {
		t.Fatalf("claimed twice: %v %v", ok, err)
	}
	if due, _ := models.DueSubscriptions(ctx, now.Add(-15*time.Minute), 10); len(due) != 0 {
		t.Fatalf("due once read: %+v", due)
	}
	cursor := "file-digest"
	must(models.FinishSubscriptionRead(ctx, link.ID, "digest-1", &cursor, ""))
	must(models.FinishSubscriptionRead(ctx, link.ID, "digest-1", nil, "The address took too long to answer."))
	subs, _ = models.SubscriptionsOf(ctx, sam)
	if subs[0].Cursor != cursor || subs[0].Error != "The address took too long to answer." || subs[0].ReadAt == nil {
		t.Fatalf("after reads %+v", subs[0])
	}

	// Only an address's events on its calendar go when they leave its file.
	event := func(calendar uuid.UUID, source string) uuid.UUID {
		id := uuid.New()
		_, err := db.Exec(`INSERT INTO events (id, calendar_id, title, start_at, end_at, ical_uid, source, created_by)
			VALUES ($1, $2, 'x', NOW(), NOW() + interval '1 hour', $3, $4, $5)`, id, calendar, id.String(), source, sam)
		must(err)
		return id
	}
	personal, err := models.PersonalCalendar(ctx, db, sam, uuid.New(), "UTC")
	must(err)
	gone, kept, own := event(cal, "subscribed"), event(cal, "subscribed"), event(personal, "subscribed")
	must(models.KeepSyncRefs(ctx, []models.SyncRef{{LinkID: link.ID, RemoteID: "digest-1", ExternalID: "gone", EventID: gone, ETag: "v1"}}))
	must(models.DeleteSubscribedEvents(ctx, cal, []uuid.UUID{gone, own}))
	var left int
	must(db.QueryRow(`SELECT count(*) FROM events WHERE id = ANY($1::uuid[])`, "{"+strings.Join([]string{gone.String(), kept.String(), own.String()}, ",")+"}").Scan(&left))
	if left != 2 {
		t.Fatalf("%d of three left; want the one on another calendar and the one still in the file", left)
	}
	refs, err := models.SyncRefsOf(ctx, link.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("a deleted event's ref stayed: %+v %v", refs, err)
	}

	// Someone else can't delete it; Sam can, with everything on it.
	if ok, err := models.DeleteSubscribedCalendar(ctx, cal, maya); err != nil || ok {
		t.Fatalf("Maya deleted it: %v %v", ok, err)
	}
	if ok, err := models.DeleteSubscribedCalendar(ctx, personal, sam); err != nil || ok {
		t.Fatalf("a personal calendar deleted as a subscription: %v %v", ok, err)
	}
	if ok, err := models.DeleteSubscribedCalendar(ctx, cal, sam); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	must(db.QueryRow(`SELECT count(*) FROM events WHERE calendar_id = $1`, cal).Scan(&left))
	if left != 0 {
		t.Fatalf("%d events left on a deleted subscription", left)
	}
	if subs, _ := models.SubscriptionsOf(ctx, sam); len(subs) != 0 {
		t.Fatalf("a subscription without its calendar is listed: %+v", subs)
	}
	if due, _ := models.DueSubscriptions(ctx, now.Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("a subscription without its calendar is due: %+v", due)
	}

	// The same address again takes the row its calendar left.
	next := uuid.New()
	must(models.SubscribedCalendar(ctx, next, sam, "Team", "", "UTC"))
	sub.CalendarID = &next
	if added, err := models.AddSubscription(ctx, sub); err != nil || !added {
		t.Fatalf("added again %v %v", added, err)
	}
	must(models.DropSubscription(ctx, link.ID, "digest-1"))
	if subs, _ := models.SubscriptionsOf(ctx, sam); len(subs) != 0 {
		t.Fatalf("dropped but listed: %+v", subs)
	}
}
