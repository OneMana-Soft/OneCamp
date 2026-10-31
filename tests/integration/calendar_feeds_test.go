//go:build integration
// +build integration

package integration_test

// Calendars' secret feeds (calendar B8) against real Postgres 12: the
// migration up, down and up; a feed found by its token's hash, kept as the
// text feeds made before keep, one live at a time for a person and a
// calendar, and none once revoked.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestCalendarFeedsMigrationUpDownUp|TestCalendarFeeds)$' -v

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/akashc777/OneCamp/tests/integration"
)

func feedsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_feeds."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_feeds %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarFeedsMigrationUpDownUp(t *testing.T) {
	up, down := feedsMigration(t, "up"), feedsMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
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
		for _, name := range []string{"calendar_feeds", "calendar_feeds_token_idx", "calendar_feeds_live_idx"} {
			if got := tableExists(t, env.PG, name); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, name, got)
			}
		}
	}
}

func TestCalendarFeeds(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
	// The feed model runs on the server's pool (models/pgexec): this test's database.
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	sam := uuid.New()
	_, err := db.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, sam, sam.String()+"@example.test", "sam", "Sam")
	must(err)
	cal, err := models.PersonalCalendar(ctx, db, sam, uuid.New(), "UTC")
	must(err)

	feed := func(token, scope string) models.Feed {
		return models.Feed{ID: uuid.New(), CalendarID: cal, PersonID: sam, TokenHash: helpers.LinkSecretHash(token), Scope: scope}
	}
	first := feed("first-secret", models.FeedFull)
	must(models.InsertFeed(ctx, first))
	got, err := models.FeedByHash(ctx, helpers.LinkSecretHash("first-secret"))
	if err != nil || got == nil || got.ID != first.ID || got.Scope != models.FeedFull {
		t.Fatalf("by hash: %+v %v", got, err)
	}
	// The hash is kept as the hex text a feed made before keeps
	// (apiTokenBusiness.HashToken's), so every link made before is found.
	var stored string
	must(db.QueryRow(`SELECT token_hash FROM calendar_feeds WHERE id = $1`, first.ID).Scan(&stored))
	if stored != apiTokenBusiness.HashToken("first-secret") {
		t.Fatalf("the token is kept as %q, not as feeds made before keep its hash", stored)
	}
	// One live feed for a person and a calendar.
	if err := models.InsertFeed(ctx, feed("second-secret", models.FeedBusy)); err == nil {
		t.Fatal("a second live feed was kept beside the first")
	}
	// Revoked: found by neither its hash nor as live; a new one is.
	must(models.RevokeFeeds(ctx, cal, sam))
	if got, err := models.FeedByHash(ctx, helpers.LinkSecretHash("first-secret")); err != nil || got != nil {
		t.Fatalf("a revoked feed was found: %+v %v", got, err)
	}
	second := feed("second-secret", models.FeedBusy)
	must(models.InsertFeed(ctx, second))
	live, err := models.LiveFeed(ctx, cal, sam)
	if err != nil || live == nil || live.ID != second.ID || live.Scope != models.FeedBusy {
		t.Fatalf("live %+v %v", live, err)
	}
	// The calendar goes, and its feeds with it.
	_, err = db.Exec(`DELETE FROM calendars WHERE id = $1`, cal)
	must(err)
	if live, _ := models.LiveFeed(ctx, cal, sam); live != nil {
		t.Fatalf("a deleted calendar's feed: %+v", live)
	}
}
