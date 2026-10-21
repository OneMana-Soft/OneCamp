//go:build integration

package models

// A doc's views by day against Postgres 12 with every migration (C6-3): a
// reload within the throttle counts once, members and visitors stay apart,
// members add up across days, and old days are swept.
// Run: go test -tags=integration ./models/postgres/ResourceView/ -run Days -v

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestViewDays(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	doc, maya, sam := uuid.NewString(), uuid.NewString(), uuid.NewString()
	today := time.Now().UTC()
	yesterday := today.AddDate(0, 0, -1)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Maya opens it three times in a row: one view. Sam yesterday and today.
	for i := 0; i < 3; i++ {
		must(RecordDay(ctx, ResourceDoc, doc, SourceMember, maya, today))
	}
	must(RecordDay(ctx, ResourceDoc, doc, SourceMember, sam, yesterday))
	must(RecordDay(ctx, ResourceDoc, doc, SourceMember, sam, today))
	// Two visitors to the page today, one of them twice past the throttle.
	must(RecordDay(ctx, ResourceDoc, doc, SourcePage, "k1", today))
	must(RecordDay(ctx, ResourceDoc, doc, SourcePage, "k2", today))
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE resource_view_days SET last_at = NOW() - interval '31 minutes' WHERE resource_uuid = $1 AND viewer_key = 'k1'`, doc)
	must(err)
	must(RecordDay(ctx, ResourceDoc, doc, SourcePage, "k1", today))

	rows, err := DaySeries(ctx, ResourceDoc, doc, yesterday)
	must(err)
	day, prev := today.Format("2006-01-02"), yesterday.Format("2006-01-02")
	want := []DayCount{
		{Day: prev, Source: SourceMember, Views: 1, Viewers: 1},
		{Day: day, Source: SourceMember, Views: 2, Viewers: 2},
		{Day: day, Source: SourcePage, Views: 3, Viewers: 2},
	}
	if len(rows) != len(want) {
		t.Fatalf("series %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("series[%d] %+v, want %+v", i, rows[i], want[i])
		}
	}
	if n, err := DistinctMembers(ctx, ResourceDoc, doc, yesterday); err != nil || n != 2 {
		t.Fatalf("members %d %v", n, err)
	}

	// Sweeping before today takes yesterday's row and leaves today's.
	n, err := PruneDays(ctx, today, 100)
	must(err)
	if n < 1 {
		t.Fatalf("swept %d", n)
	}
	rows, err = DaySeries(ctx, ResourceDoc, doc, yesterday)
	must(err)
	if len(rows) != 2 || rows[0].Day != day {
		t.Fatalf("after the sweep %+v", rows)
	}

	// A member's choice defaults to recording.
	var def string
	must(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT column_default FROM information_schema.columns
		 WHERE table_name = 'users_notification_preferences' AND column_name = 'record_views'`).Scan(&def))
	if def != "true" {
		t.Fatalf("record_views defaults to %q", def)
	}
}
