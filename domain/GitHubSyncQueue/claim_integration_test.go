//go:build integration

package Domain

import (
	"context"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// Workers claiming at once never take the same queue item, and between them
// take every due one. The worker used to read the pending items and then mark
// each processing with WHERE id = $1 and no status guard, so two API
// instances could both process one item: a status pushed to GitHub twice, a
// comment posted twice.
func TestSyncQueueItemsAreClaimedOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	user, project := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name) VALUES ($1, $2)`, project, "sync "+project.String()[:8]); err != nil {
		t.Fatal(err)
	}
	const items = 60
	for i := 0; i < items; i++ {
		task := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, user); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO github_sync_queue (task_id, sync_type, payload) VALUES ($1, 'status', '{}')`, task); err != nil {
			t.Fatal(err)
		}
	}
	// One item not due yet stays where it is.
	later := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, later, project, user); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO github_sync_queue (task_id, sync_type, payload, next_retry_at) VALUES ($1, 'name', '{}', NOW() + INTERVAL '1 hour')`, later); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	claimed := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				got, err := ClaimGitHubSyncQueueItems(ctx, 3)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, it := range got {
					claimed[it.ID]++
					if it.Status != "processing" || it.Attempts != 1 {
						t.Errorf("item %s claimed as %q with %d attempts; want processing, 1", it.ID, it.Status, it.Attempts)
					}
					if it.TaskID == later {
						t.Errorf("an item not due yet was claimed")
					}
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(claimed) != items {
		t.Errorf("claimed %d distinct items; want all %d due", len(claimed), items)
	}
	for id, n := range claimed {
		if n > 1 {
			t.Errorf("item %s was claimed %d times", id, n)
		}
	}
	var processing int
	if err := env.PG.QueryRow(`SELECT count(*) FROM github_sync_queue WHERE status = 'processing' AND attempts = 1`).Scan(&processing); err != nil {
		t.Fatal(err)
	}
	if processing != items {
		t.Errorf("%d items are processing with one attempt; want %d", processing, items)
	}
}
