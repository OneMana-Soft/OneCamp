//go:build integration

package business

// Archiving completed tasks against a real Postgres 12 (every migration
// applied) and a real Dgraph with the production schema.
// Run: go test -tags=integration ./business/Archive/ -run TestArchivingCompletedTasks -v

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestArchivingCompletedTasksMovesOnlyTasksClosedBeforeTheCutoff(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	dg := integration.SetupDgraph(t)

	now := time.Now().UTC()
	days := func(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }
	type task struct {
		name, status string
		created      int // days ago
		closedSince  int // days ago; 0: no date recorded
		deleted      bool
		archive      bool
	}
	tasks := []task{
		{name: "done long ago", status: "done", created: 400, closedSince: 300, archive: true},
		{name: "canceled, no date recorded", status: "canceled", created: 400, archive: true},
		{name: "done yesterday", status: "done", created: 400, closedSince: 1},
		{name: "open and old", status: "todo", created: 400},
		{name: "done but new", status: "done", created: 5, closedSince: 2},
		{name: "done and deleted", status: "done", created: 400, closedSince: 300, deleted: true},
	}
	ids := map[string]string{}
	var nodes []map[string]any
	for _, tk := range tasks {
		id := uuid.NewString()
		ids[tk.name] = id
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
			`INSERT INTO tasks (id, created_at, updated_at) VALUES ($1, $2, $2)`, id, now.AddDate(0, 0, -tk.created)); err != nil {
			t.Fatalf("seed %s: %v", tk.name, err)
		}
		n := map[string]any{"task_uuid": id, "task_name": tk.name, "task_status": tk.status,
			"task_created_at": days(tk.created), "dgraph.type": "Task"}
		if tk.closedSince > 0 {
			n["task_status_since"] = days(tk.closedSince)
		}
		if tk.deleted {
			n["task_deleted_at"] = days(10)
		}
		nodes = append(nodes, n)
	}
	dg.Mutate(t, nodes)

	archived, attempted, got, err := archiveTasks(ctx, now.AddDate(0, 0, -90), true)
	if err != nil {
		t.Fatalf("archive completed tasks: %v", err)
	}
	var want []string
	for _, tk := range tasks {
		if tk.archive {
			want = append(want, ids[tk.name])
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if archived != int64(len(want)) || attempted != archived || len(got) != len(want) {
		t.Fatalf("archived %d of %d: %v, want %v", archived, attempted, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("archived %v, want %v", got, want)
		}
	}
	for _, tk := range tasks {
		var deleted bool
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
			`SELECT deleted_at IS NOT NULL FROM tasks WHERE id = $1`, ids[tk.name]).Scan(&deleted); err != nil {
			t.Fatal(err)
		}
		if deleted != tk.archive {
			t.Errorf("%s: archived=%v, want %v", tk.name, deleted, tk.archive)
		}
	}

	// A second run finds nothing more to move.
	if n, _, again, err := archiveTasks(ctx, now.AddDate(0, 0, -90), true); err != nil || n != 0 || len(again) != 0 {
		t.Errorf("second run archived %d (%v), err %v", n, again, err)
	}
}
