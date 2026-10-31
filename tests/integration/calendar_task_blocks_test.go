//go:build integration
// +build integration

package integration_test

// Task blocks (calendar B1, gcal-new-task-blocks) against real Postgres 12:
// the migration up, down and up, and twice over; a block keeps its task
// through the store's read, range and update; a task names a block alone,
// held by the table itself, on an insert and on an update.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestCalendarTaskBlocksMigrationUpDownUp|TestCalendarTaskBlocks)$' -v

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

func taskBlocksMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_task_blocks."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one calendar_task_blocks %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCalendarTaskBlocksMigrationUpDownUp(t *testing.T) {
	up, down := taskBlocksMigration(t, "up"), taskBlocksMigration(t, "down")
	if strings.Contains(up, "gen_random_uuid") {
		t.Fatal("ids are made in Go")
	}
	env := integration.SetupEnv(t)
	constraint := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = 'events_task_only_on_blocks'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {up, true}, {down, false}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if got := columnExists(t, env, "events", "task_uuid"); got != step.want {
			t.Fatalf("step %d: events.task_uuid exists = %v", i, got)
		}
		if got := tableExists(t, env.PG, "events_task_idx"); got != step.want {
			t.Fatalf("step %d: events_task_idx exists = %v", i, got)
		}
		if got := constraint(); got != step.want {
			t.Fatalf("step %d: the check exists = %v", i, got)
		}
	}
}

func TestCalendarTaskBlocks(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	db := env.PG
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
	start := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	task, block := uuid.New(), uuid.New()
	insert := func(id uuid.UUID, kind string, taskID *uuid.UUID) error {
		_, err := db.Exec(`INSERT INTO events (id, calendar_id, kind, title, created_by, ical_uid, start_at, end_at, task_uuid)
			VALUES ($1, $2, $3, 'Write the brief', $4, $5, $6, $7, $8)`, id, cal, kind, sam, id.String()+"@onecamp.test", start, end, taskID)
		return err
	}
	must(insert(block, "task_block", &task))

	// The store's read and range carry the block's task.
	got, err := models.Get(ctx, db, block)
	must(err)
	if got == nil || got.Kind != "task_block" || got.TaskUUID == nil || *got.TaskUUID != task {
		t.Fatalf("read: %+v", got)
	}
	rows, err := models.RangeOf(ctx, db, sam, models.Window{From: start.Add(-time.Hour), To: end.Add(time.Hour), FromDate: "2026-10-22", ToDate: "2026-10-23"}, 10)
	must(err)
	if len(rows) != 1 || rows[0].TaskUUID == nil || *rows[0].TaskUUID != task {
		t.Fatalf("range: %+v", rows)
	}

	// A task names a block alone: the table refuses it on an event.
	if err := insert(uuid.New(), "event", &task); err == nil {
		t.Fatal("an event was kept naming a task")
	}

	// An update keeps the task, and can't keep it on another kind.
	got.Title = "Brief, first draft"
	_, err = models.Update(ctx, db, got, nil)
	must(err)
	again, err := models.Get(ctx, db, block)
	must(err)
	if again.Title != "Brief, first draft" || again.TaskUUID == nil || *again.TaskUUID != task {
		t.Fatalf("after an update: %+v", again)
	}
	again.Kind = "event"
	if _, err := models.Update(ctx, db, again, nil); err == nil {
		t.Fatal("an event was kept naming a task, by an update")
	}
	again, err = models.Get(ctx, db, block)
	must(err)
	again.Kind, again.TaskUUID = "event", nil
	_, err = models.Update(ctx, db, again, nil)
	must(err)
	plain, err := models.Get(ctx, db, block)
	must(err)
	if plain.Kind != "event" || plain.TaskUUID != nil {
		t.Fatalf("made an event, it lets the task go: %+v", plain)
	}

	// A task's blocks are found by its index.
	must(insert(uuid.New(), "task_block", &task))
	var n int
	must(db.QueryRow(`SELECT count(*) FROM events WHERE task_uuid = $1`, task).Scan(&n))
	if n != 1 {
		t.Fatalf("the task's blocks: %d", n)
	}
}
