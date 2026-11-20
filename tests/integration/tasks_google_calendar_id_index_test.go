//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Migration 603: the index on the tasks synced to Google comes down and goes
// up again, on a database every migration has run on (the harness applies
// them all from empty). The up builds it outside a transaction without
// blocking writes; the down runs inside one, as sqlx-cli 0.8 runs a revert
// whatever its first line says (587's down is the precedent).
func TestTasksGoogleCalendarIdIndexMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/603_tasks_google_calendar_id_index." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("its %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	if !strings.HasPrefix(up, "-- no-transaction\n") || !strings.Contains(up, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("its up isn't built without blocking writes:\n%s", up)
	}
	env := integration.SetupEnv(t)
	has := func() bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'tasks' AND indexname = 'idx_tasks_google_calendar_id'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	// The down in a transaction, as a revert runs; the up outside one.
	inTx := func(sql string) error {
		tx, err := env.PG.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(sql); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		var err error
		switch step.sql {
		case "":
		case down:
			err = inTx(step.sql)
		default:
			_, err = env.PG.Exec(step.sql)
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if has() != step.want {
			t.Fatalf("step %d: the index is there = %v, want %v", i, !step.want, step.want)
		}
	}
}

// Every calendar listing asks which fetched Google events are a task's copy.
// Under a generic plan (an array parameter, as the pool's prepared statement
// runs it) the hot tasks table is read through idx_tasks_google_calendar_id,
// not by a scan of every live task: the query repeats the partial index's
// predicate. The tasks are a workspace's shape, many live and a few synced,
// since on an empty table every index costs the same.
func TestTheSyncedTaskLookupReadsItsIndex(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	tx, err := env.PG.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	maker, project := uuid.New(), uuid.New()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'ida@synced.test', 'ida', 'Ida', NOW(), NOW())`, []any{maker}},
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Synced', $2)`, []any{project, maker}},
		{`INSERT INTO tasks (id, project_id, created_by) SELECT md5('unsynced' || i)::uuid, $1, $2 FROM generate_series(1, 5000) i`, []any{project, maker}},
		{`INSERT INTO tasks (id, project_id, created_by, task_google_calendar_id) SELECT md5('synced' || i)::uuid, $1, $2, 'gcal-' || i FROM generate_series(1, 3) i`, []any{project, maker}},
		{`ANALYZE tasks`, nil},
		{`SET LOCAL plan_cache_mode = force_generic_plan`, nil},
		{`PREPARE synced_task_copies (text[]) AS ` + taskDomain.TaskCopiesAmongQueryForTest, nil},
	} {
		if _, err := tx.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}
	defer func() { _, _ = tx.ExecContext(ctx, `DEALLOCATE synced_task_copies`) }()
	rows, err := tx.QueryContext(ctx, `EXPLAIN EXECUTE synced_task_copies ('{gcal-a,gcal-b}')`)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	_ = rows.Close()
	if !strings.Contains(strings.Join(plan, "\n"), "idx_tasks_google_calendar_id") {
		t.Errorf("the tasks synced to Google are found by a scan:\n%s", strings.Join(plan, "\n"))
	}
}
