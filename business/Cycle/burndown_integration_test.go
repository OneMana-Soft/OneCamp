//go:build integration

package business

// What a completed cycle keeps of the tasks it left unfinished, against
// Postgres 12 with every migration.
// Run: go test -tags=integration ./business/Cycle/ -run 'TestUnfinished' -v

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestUnfinishedTasksStayInTheCycleTheyLeft(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user, project := uuid.New(), uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'lead@example.com', NOW(), NOW())`, user); err != nil {
		t.Fatal(err)
	}
	task := func() uuid.UUID {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO tasks (id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	start := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	a, err := cycleModel.Create(project, "", start, start.AddDate(0, 0, 14), user)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cycleModel.Create(project, "", start.AddDate(0, 0, 14), start.AddDate(0, 0, 28), user)
	if err != nil {
		t.Fatal(err)
	}
	finished, carried, dropped := task(), task(), task()
	for _, id := range []uuid.UUID{finished, carried, dropped} {
		if err := cycleModel.SetTask(id, &a.Id); err != nil {
			t.Fatal(err)
		}
	}
	joined := map[string]time.Time{}
	before, err := cycleModel.MembersOf(a.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range before {
		joined[m.TaskUUID] = m.AddedAt
	}

	if ok, err := cycleModel.Complete(a.Id, 1, []string{carried.String(), dropped.String()}, &b.Id); err != nil || !ok {
		t.Fatalf("complete: %v %v", ok, err)
	}
	after, err := cycleModel.MembersOf(a.Id)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(after, func(i, j int) bool { return !after[i].Unfinished && after[j].Unfinished })
	if len(after) != 3 || after[0].TaskUUID != finished.String() || after[0].Unfinished || !after[1].Unfinished || !after[2].Unfinished {
		t.Fatalf("the finished task, then the two it left unfinished: %+v", after)
	}
	for _, m := range after {
		if !m.AddedAt.Equal(joined[m.TaskUUID]) {
			t.Errorf("%s joined at %v, kept as %v", m.TaskUUID, joined[m.TaskUUID], m.AddedAt)
		}
	}
	if next, err := cycleModel.MembersOf(b.Id); err != nil || len(next) != 2 || next[0].Unfinished {
		t.Fatalf("the next cycle has both carried tasks as its own: %+v %v", next, err)
	}

	// Put back in the cycle it left: listed once, as a member.
	if err := cycleModel.SetTask(carried, &a.Id); err != nil {
		t.Fatal(err)
	}
	again, err := cycleModel.MembersOf(a.Id)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, m := range again {
		if m.TaskUUID == carried.String() {
			seen++
			if m.Unfinished {
				t.Errorf("back in the cycle, not unfinished: %+v", m)
			}
		}
	}
	if seen != 1 {
		t.Errorf("listed %d times, want once", seen)
	}

	// Completed with nowhere to carry: the task leaves cycles, the trace stays.
	c, err := cycleModel.Create(project, "", start.AddDate(0, 0, 28), start.AddDate(0, 0, 35), user)
	if err != nil {
		t.Fatal(err)
	}
	left := task()
	if err := cycleModel.SetTask(left, &c.Id); err != nil {
		t.Fatal(err)
	}
	if ok, err := cycleModel.Complete(c.Id, 0, []string{left.String()}, nil); err != nil || !ok {
		t.Fatalf("complete without carrying: %v %v", ok, err)
	}
	if m, err := cycleModel.MembersOf(c.Id); err != nil || len(m) != 1 || !m[0].Unfinished {
		t.Fatalf("the task it left: %+v %v", m, err)
	}
	if in, err := cycleModel.CycleOf(left); err != nil || in != nil {
		t.Fatalf("the task is in no cycle now: %+v %v", in, err)
	}
}
