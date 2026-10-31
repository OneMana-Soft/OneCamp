//go:build integration

package business

// A project's cycles from Linear against Postgres 12 with every migration.
// Run: go test -tags=integration ./business/Import/ -run TestImportedCycles -v

import (
	"context"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// L-2: the cycles that held a project's issues come over with their names,
// dates and how they ended; a cycle from another team that shares days folds
// into the one running when it starts, and the report says so; each task goes
// back into its cycle; the project goes on making cycles as its team did; and
// a rollback takes away the cycles the import made that no task in use holds.
// Before, every cycle was dropped.
func TestImportedCycles(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	project, by := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, by)
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, project, by)
	task := func() uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO tasks (id) VALUES ($1)`, id)
		return id
	}

	now := time.Now().UTC().Truncate(time.Second)
	week := 7 * 24 * time.Hour
	closedAt := now.Add(-week)
	makeCycles(ctx, job, &importProvider.SourceProject{
		SourceID: "proj",
		Cycles: []importProvider.SourceCycle{
			{SourceID: "c12", Name: "Cycle 12", StartsAt: now.Add(-3 * week), EndsAt: now.Add(-week), CompletedAt: &closedAt, Done: 2},
			{SourceID: "c13", Name: "Polish", StartsAt: now.Add(-week), EndsAt: now.Add(week)},
			// Another team's, which shares days with Cycle 12.
			{SourceID: "mob-5", Name: "Cycle 5", StartsAt: now.Add(-2 * week), EndsAt: now, CompletedAt: &closedAt, Done: 1},
		},
		CycleSchedule: &importProvider.SourceCycleSchedule{Weeks: 2, Upcoming: 2, RollOver: true, Anchor: now.Add(-week)},
	}, project, by)

	cycles, err := cycleModel.List(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 4 {
		t.Fatalf("the two kept cycles and the two the schedule keeps ready after them, got %d: %+v", len(cycles), cycles)
	}
	closed, running := cycles[0], cycles[1]
	if closed.Name != "Cycle 12" || closed.CompletedAt == nil || !closed.CompletedAt.Equal(closedAt) || closed.DoneCount == nil || *closed.DoneCount != 3 {
		t.Errorf("Cycle 12 stays closed when Linear closed it, with its two finished and the folded cycle's one: %+v", closed)
	}
	if running.Name != "Polish" || running.CompletedAt != nil || !running.StartsAt.Equal(now.Add(-week)) || !running.EndsAt.Equal(now.Add(week)) {
		t.Errorf("Polish runs on its own dates: %+v", running)
	}
	if !cycles[2].StartsAt.Equal(now.Add(week)) || !cycles[3].StartsAt.Equal(now.Add(3*week)) {
		t.Errorf("the schedule makes the next cycles on the team's weeks, after the last that came: %+v", cycles[2:])
	}

	a, b, c, d := task(), task(), task(), task()
	for id, cycle := range map[uuid.UUID]string{a: "c13", b: "mob-5", c: "c99", d: "c99"} {
		placeInCycle(ctx, job, &importProvider.SourceTask{SourceID: id.String(), ProjectSourceID: "proj", CycleSourceID: cycle}, id)
	}
	in := func(task uuid.UUID) string {
		t.Helper()
		got, err := cycleModel.CycleOf(task)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			return ""
		}
		return got.Name
	}
	if in(a) != "Polish" || in(b) != "Cycle 12" || in(c) != "" {
		t.Fatalf("each task in its cycle, a folded cycle's in the one it folded into, one whose cycle wasn't made in none: %q %q %q", in(a), in(b), in(c))
	}

	rows, err := importModels.ListErrors(ctx, job.Id, importModels.SeverityWarning, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]int{}
	for _, r := range rows {
		if code, _ := r["code"].(*string); code != nil {
			codes[*code]++
		}
	}
	if codes["CYCLES_FOLDED"] != 1 || codes["CYCLE_NOT_KEPT"] != 1 || len(rows) != 2 {
		t.Fatalf("the fold said once for the project, and the cycle not made said once for its two tasks: %v", codes)
	}

	// Its tasks are taken away; someone's own task in Polish keeps it.
	exec(`UPDATE tasks SET deleted_at = NOW() WHERE id = ANY($1::uuid[])`, pq.Array([]string{a.String(), b.String(), c.String(), d.String()}))
	mine := task()
	if err := cycleModel.SetTask(mine, &running.Id); err != nil {
		t.Fatal(err)
	}
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteUnusedMade(ctx, tx, job.Id, importModels.EntityCycle); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	left, err := cycleModel.List(project)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range left {
		names = append(names, c.Name)
	}
	if len(left) != 3 || left[0].Id != running.Id {
		t.Fatalf("Cycle 12 goes; Polish, which someone's task is in, stays, as do the schedule's own: %q", names)
	}

	// It takes Cycle 12 away as a person's delete does (V-78): kept, with its
	// task in it, so undoing the rollback (the tasks back from the trash, the
	// cycle restored) has each task in its cycle again.
	gone := keptDeleted(t, "project_cycles", "project_uuid", project)
	if len(gone) != 1 {
		t.Fatalf("Cycle 12 is kept for its restore: %d kept, so the rollback's delete is hard", len(gone))
	}
	exec(`UPDATE tasks SET deleted_at = NULL WHERE id = ANY($1::uuid[])`, pq.Array([]string{a.String(), b.String(), c.String(), d.String()}))
	if _, err := cycleModel.Restore(ctx, gone[0]); err != nil {
		t.Fatal(err)
	}
	if in(a) != "Polish" || in(b) != "Cycle 12" {
		t.Errorf("undone, each task is in its cycle again: %q %q", in(a), in(b))
	}
}
