//go:build integration

package models

// A repeat's zone and day, against a real Postgres 12 with every migration
// applied (200 adds them, and gives repeats set before it their creator's
// zone where one is known).
// Run: go test -tags=integration ./models/postgres/TaskRecurrence/ -v

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestARepeatKeepsItsZoneAndDayAsItMoves(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	person := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
		person, person.String()+"@example.test", "p-"+person.String()[:8], "p"); err != nil {
		t.Fatal(err)
	}
	first, next := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{first, next} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO tasks (id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}

	set, err := Set(first, "FREQ=MONTHLY", ModeSchedule, "Asia/Kolkata", 31, person)
	if err != nil || set.TimeZone != "Asia/Kolkata" || set.AnchorDay != 31 {
		t.Fatalf("set: %+v %v", set, err)
	}
	taken, err := Take(first)
	if err != nil || taken == nil || taken.TimeZone != "Asia/Kolkata" || taken.AnchorDay != 31 {
		t.Fatalf("take: %+v %v", taken, err)
	}
	taken.TaskUUID = next
	if err := Put(taken); err != nil {
		t.Fatal(err)
	}
	moved, err := Get(next)
	if err != nil || moved == nil || moved.TimeZone != "Asia/Kolkata" || moved.AnchorDay != 31 || moved.Rule != "FREQ=MONTHLY" {
		t.Fatalf("the next occurrence's repeat: %+v %v", moved, err)
	}
	// Set again without a day or zone: both are what was asked for, not kept.
	if again, err := Set(next, "FREQ=WEEKLY", ModeSchedule, "", 0, person); err != nil || again.TimeZone != "" || again.AnchorDay != 0 {
		t.Fatalf("set again: %+v %v", again, err)
	}
	// A day the column refuses.
	if _, err := Set(next, "FREQ=MONTHLY", ModeSchedule, "", 32, person); err == nil {
		t.Error("day 32 was stored")
	}
}

// What migration 200 does to repeats set before it: their creator's zone,
// where they gave a real one for quiet hours; UTC otherwise.
func TestOlderRepeatsTakeTheirCreatorsZone(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	type who struct {
		zone *string
		want string
	}
	india, bogus := "Asia/Kolkata", "Mars/Olympus_Mons"
	people := []who{{&india, india}, {&bogus, ""}, {nil, ""}}
	var tasks []uuid.UUID
	for i, p := range people {
		person, task := uuid.New(), uuid.New()
		tasks = append(tasks, task)
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
			`INSERT INTO users (id, email_id, username, display_name) VALUES ($1, $2, $3, $4)`,
			person, person.String()+"@example.test", "p-"+person.String()[:8], "p"); err != nil {
			t.Fatal(err)
		}
		if p.zone != nil {
			if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
				`INSERT INTO users_notification_preferences (user_id, quiet_hours_tz, unsubscribe_token) VALUES ($1, $2, $3)`,
				person, *p.zone, uuid.NewString()); err != nil {
				t.Fatalf("person %d: %v", i, err)
			}
		}
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO tasks (id) VALUES ($1)`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
			`INSERT INTO task_recurrences (task_uuid, rule, mode, created_by) VALUES ($1, 'FREQ=WEEKLY', 'schedule', $2)`,
			task, person); err != nil {
			t.Fatal(err)
		}
	}
	// The migration's backfill, run again over these rows (it is idempotent).
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE task_recurrences r
		   SET time_zone = p.quiet_hours_tz
		  FROM users_notification_preferences p
		 WHERE p.user_id = r.created_by
		   AND r.time_zone = ''
		   AND p.quiet_hours_tz IN (SELECT name FROM pg_timezone_names)`); err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		r, err := Get(task)
		if err != nil || r == nil || r.TimeZone != people[i].want {
			t.Errorf("person %d: %+v %v, want zone %q", i, r, err, people[i].want)
		}
	}
}
