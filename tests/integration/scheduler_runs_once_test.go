//go:build integration
// +build integration

package integration_test

// Scheduled jobs (reminders, scheduled messages, Later) run once, against
// real Postgres and Redis (BE-BUG-15).
//
// A worker used to claim fifty due jobs at once and run them in a line, each
// for up to a minute, while another worker could take any lock older than five
// minutes: the jobs at the end of a slow batch ran twice. And a job that did
// its work but wasn't marked done after was run again once its lock was old.
// Now:
//   - two workers draining at once run each job exactly once;
//   - a run whose marking was lost is finished, not run again;
//   - a repeating job's next run is a run of its own, and runs.
//
// Run: go test -tags=integration ./tests/integration/ -run TestScheduledJobsRunOnce -v

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestScheduledJobsRunOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)

	kind := "test.runs_once." + uuid.NewString()[:8]
	var mu sync.Mutex
	runs := map[uuid.UUID]int{}
	schedulerBusiness.RegisterJobHandler(kind, func(_ context.Context, job *jobModel.ScheduledJob) error {
		time.Sleep(15 * time.Millisecond) // a send takes a while
		mu.Lock()
		runs[job.Id]++
		mu.Unlock()
		return nil
	})
	ranTimes := func(id uuid.UUID) int {
		mu.Lock()
		defer mu.Unlock()
		return runs[id]
	}
	status := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var s string
		if err := env.PG.QueryRow(`SELECT status FROM scheduled_jobs WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	owner := uuid.New()
	enqueue := func(t *testing.T, recurrence *string) uuid.UUID {
		t.Helper()
		id, err := schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{
			JobType: kind, UserUUID: owner, PayloadJSON: "{}", RunAt: time.Now().Add(-time.Minute), Recurrence: recurrence,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("two workers draining at once run each job once", func(t *testing.T) {
		var ids []uuid.UUID
		for range 40 {
			ids = append(ids, enqueue(t, nil))
		}
		var wg sync.WaitGroup
		total := make(chan int, 2)
		for _, worker := range []string{"worker-a", "worker-b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				total <- schedulerBusiness.DrainDueJobs(ctx, worker)
			}()
		}
		wg.Wait()
		close(total)
		sum := 0
		for n := range total {
			sum += n
		}
		if sum != len(ids) {
			t.Errorf("the workers ran %d jobs between them, want %d", sum, len(ids))
		}
		for _, id := range ids {
			if n := ranTimes(id); n != 1 {
				t.Errorf("job %s ran %d times", id, n)
			}
			if s := status(t, id); s != jobModel.StatusDone {
				t.Errorf("job %s is %s", id, s)
			}
		}
	})

	t.Run("a run whose marking was lost is finished, not run again", func(t *testing.T) {
		id := enqueue(t, nil)
		schedulerBusiness.DrainDueJobs(ctx, "worker-a")
		if n := ranTimes(id); n != 1 {
			t.Fatalf("ran %d times", n)
		}
		// It did its work, and then wasn't marked done: its worker died, and
		// its lock is old enough to take.
		if _, err := env.PG.Exec(`UPDATE scheduled_jobs SET status = 'running', locked_at = NOW() - INTERVAL '10 minutes', locked_by = 'gone' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		schedulerBusiness.DrainDueJobs(ctx, "worker-b")
		if n := ranTimes(id); n != 1 {
			t.Errorf("ran %d times, want once: a reminder or message would have gone out again", n)
		}
		if s := status(t, id); s != jobModel.StatusDone {
			t.Errorf("left %s, want done", s)
		}
	})

	t.Run("a repeating job's next run runs", func(t *testing.T) {
		daily := "FREQ=DAILY"
		id := enqueue(t, &daily)
		schedulerBusiness.DrainDueJobs(ctx, "worker-a")
		if n := ranTimes(id); n != 1 {
			t.Fatalf("ran %d times", n)
		}
		// Its next time comes.
		if _, err := env.PG.Exec(`UPDATE scheduled_jobs SET run_at = NOW() - INTERVAL '1 minute' WHERE id = $1 AND status = 'pending'`, id); err != nil {
			t.Fatal(err)
		}
		schedulerBusiness.DrainDueJobs(ctx, "worker-a")
		if n := ranTimes(id); n != 2 {
			t.Errorf("ran %d times, want twice: its second run is a run of its own", n)
		}
	})
}
