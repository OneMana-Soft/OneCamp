//go:build integration

package business

// Undoing a run when the admin's request ends part way, against a real
// Postgres 12 (every migration applied) and a real Dgraph with the production
// schema.
// Run: PKGS='./business/Archive/' integ_rel.sh <worktree> <log> '^(TestAnUndoWhoseRequestEndsStillRestoresTheWholeRun|TestAnUndoThatFailsPartWayIsPartialAndARetryFinishesIt|TestASecondUndoWaitsWhileTheFirstGoesOn|TestAnUndoLeftPastItsDeadlineIsFinished)$'
//
// Why (REGRESSION-AUDIT, 23 Oct 2026): a run archives every row of its type
// older than the policy's cutoff, in the background, however many there are.
// Its undo restores them all inside the admin's request: one Postgres
// statement, then the graph in batches of 500. The web stops waiting after
// 30 s (lib/http/requestTimeout.ts) and the proxy then ends the request, which
// cancels its context, so a big run's undo stopped part way: every row was
// live in Postgres, the posts past the cut stayed deleted in the graph that
// the app reads them from, and the run still read "completed". Pressing Undo
// again restored nothing, since Postgres had nothing left to restore, and the
// restore list reads Postgres, so it listed none of them either.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	archiveModels "github.com/akashc777/OneCamp/models/postgres/Archive"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func TestAnUndoWhoseRequestEndsStillRestoresTheWholeRun(t *testing.T) {
	env, dg, job, ids := archivedRun(t)
	ctx := context.Background()

	// The admin's request ends once the graph has taken the first batch back:
	// the web stopped waiting.
	request, leave := context.WithCancel(ctx)
	defer leave()
	swapGraphRestore(t, func(c context.Context, restored []string, inGraph func(context.Context, []string) error) error {
		began := time.Now()
		if err := inGraph(c, restored[:graphBatch]); err != nil {
			return err
		}
		t.Logf("the graph took the first %d posts back in %s", graphBatch, time.Since(began))
		leave()
		return inGraph(c, restored[graphBatch:])
	})

	count, err := UndoArchiveJob(request, job)
	t.Logf("the undo answered %d, %v", count, err)

	// However the server goes on once the request has ended, the run comes
	// back whole: every post live in Postgres and in the graph, and the run
	// marked undone.
	waitForRun(t, env, dg, job, ids, "cancelled", 0)
}

// An undo whose graph step fails part way says so: the run reads "partial"
// (part of it is back), never "completed", with why; and Undo again heals it,
// though Postgres has nothing left to restore.
func TestAnUndoThatFailsPartWayIsPartialAndARetryFinishesIt(t *testing.T) {
	env, dg, job, ids := archivedRun(t)
	ctx := context.Background()
	restoreInGraph := swapGraphRestore(t, func(c context.Context, restored []string, inGraph func(context.Context, []string) error) error {
		if err := inGraph(c, restored[:graphBatch]); err != nil {
			return err
		}
		return errors.New("the graph stopped answering")
	})

	if _, err := UndoArchiveJob(ctx, job); err == nil {
		t.Fatal("the undo whose graph step failed answered no error")
	}
	waitForRun(t, env, dg, job, ids, "partial", len(ids)-graphBatch)
	var why sql.NullString
	if err := env.PG.QueryRow(`SELECT error_message FROM archive_jobs WHERE id = $1`, job).Scan(&why); err != nil || !strings.Contains(why.String, "the graph stopped answering") {
		t.Errorf("the partly undone run says why %q (%v); want the graph's error", why.String, err)
	}

	entityRegistry["posts"] = restoreInGraph
	count, err := UndoArchiveJob(ctx, job)
	if err != nil || count != len(ids) {
		t.Fatalf("Undo again answered %d, %v; want all %d back", count, err, len(ids))
	}
	waitForRun(t, env, dg, job, ids, "cancelled", 0)
}

// A second Undo while the first is still at work waits for it. Undo from
// another tab, or by another admin, began the whole undo again, doubling a big
// run's work for up to the undo's deadline (CODE-REVIEWER-23's SF1 on R3, 24
// Oct 2026). Meanwhile the runs list says the run is being undone, so the
// card shows it restoring and offers no Undo.
func TestASecondUndoWaitsWhileTheFirstGoesOn(t *testing.T) {
	env, _, job, ids := archivedRun(t)
	ctx := context.Background()

	// The first undo has begun, another tab's, and is still at work.
	if claimed, err := archiveModels.ClaimUndo(ctx, job); err != nil || !claimed {
		t.Fatalf("begin the first undo: claimed %v, %v", claimed, err)
	}
	if !undoingInList(t, job) {
		t.Error("the runs list doesn't say the run is being undone")
	}

	count, err := UndoArchiveJob(ctx, job)
	if err == nil {
		t.Fatalf("a second undo while the first goes on answered %d restored and no error; want it refused", count)
	}
	if !errors.Is(err, ErrUndoUnderWay) {
		t.Errorf("the second undo was refused with %v; want ErrUndoUnderWay, which the web says as \"being restored\"", err)
	}
	var live int
	if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, pq.Array(ids)).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("the refused undo brought %d posts back; want none: they are the first undo's to bring back", live)
	}
}

// An undo whose server went down mid-way never finished, and no request is at
// work on it once its deadline has passed: Undo finishes it. (One that failed
// finishes on the next Undo at once: FinishUndo stops it, which
// TestAnUndoThatFailsPartWayIsPartialAndARetryFinishesIt covers.)
func TestAnUndoLeftPastItsDeadlineIsFinished(t *testing.T) {
	env, dg, job, ids := archivedRun(t)
	ctx := context.Background()

	if claimed, err := archiveModels.ClaimUndo(ctx, job); err != nil || !claimed {
		t.Fatalf("begin the undo that never finishes: claimed %v, %v", claimed, err)
	}
	past := (archiveModels.UndoDeadline + time.Minute).Seconds()
	if _, err := env.PG.Exec(`UPDATE archive_jobs SET undo_started_at = NOW() - $2 * INTERVAL '1 second' WHERE id = $1`, job, past); err != nil {
		t.Fatalf("age the undo past its deadline: %v", err)
	}
	if undoingInList(t, job) {
		t.Error("the runs list says an undo past its deadline is at work")
	}

	count, err := UndoArchiveJob(ctx, job)
	if err != nil || count != len(ids) {
		t.Fatalf("Undo of a run left past its deadline answered %d, %v; want all %d back", count, err, len(ids))
	}
	waitForRun(t, env, dg, job, ids, "cancelled", 0)
}

// undoingInList is whether the runs list, as the web reads it, says the run is
// being undone.
func undoingInList(t *testing.T, job uuid.UUID) bool {
	t.Helper()
	jobs, err := archiveModels.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("list the runs: %v", err)
	}
	raw, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["id"] == job.String() {
			return row["undoing"] == true
		}
	}
	t.Fatalf("the runs list has no run %s", job)
	return false
}

// graphBatch is how many posts the graph restores at a time
// (BulkRestorePostsInDgraph).
const graphBatch = 500

// archivedRun is a completed run that archived 1,200 posts, three of the
// graph's batches, in a real Postgres 12 and Dgraph.
func archivedRun(t *testing.T) (*integration.Env, *integration.DgraphEnv, uuid.UUID, []string) {
	t.Helper()
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	const n = 1200
	archivedAt := time.Now().UTC().AddDate(0, 0, -3)
	ids := make([]string, n)
	nodes := make([]map[string]any, n)
	for i := range ids {
		ids[i] = uuid.NewString()
		nodes[i] = map[string]any{"post_uuid": ids[i], "post_deleted_at": archivedAt.Format(time.RFC3339), "dgraph.type": "Post"}
	}
	created := archivedAt.AddDate(-1, 0, 0)
	if _, err := env.PG.Exec(`INSERT INTO posts (id, created_at, updated_at, deleted_at)
		SELECT id, $2, $2, $3 FROM unnest($1::uuid[]) AS id`, pq.Array(ids), created, archivedAt); err != nil {
		t.Fatalf("seed the archived posts: %v", err)
	}
	dg.Mutate(t, nodes)
	job := uuid.New()
	if err := archiveModels.CreateJob(ctx, job, "posts", archivedAt, nil); err != nil {
		t.Fatalf("record the run: %v", err)
	}
	if err := archiveModels.AddJobItems(ctx, job, ids); err != nil {
		t.Fatalf("record what the run archived: %v", err)
	}
	if err := archiveModels.FinishJob(ctx, job, "completed", archivedAt, n, n, 0, nil); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	return env, dg, job, ids
}

// swapGraphRestore has the posts' graph restore go through step, which is
// handed the real one, until the test ends; it answers the posts' entry with
// the real one, for a test to put back.
func swapGraphRestore(t *testing.T, step func(c context.Context, restored []string, inGraph func(context.Context, []string) error) error) entityOps {
	t.Helper()
	ops := entityRegistry["posts"]
	t.Cleanup(func() { entityRegistry["posts"] = ops })
	swapped := ops
	swapped.RestoreDgraphFn = func(c context.Context, restored []string) error {
		if len(restored) <= graphBatch {
			return ops.RestoreDgraphFn(c, restored)
		}
		return step(c, restored, ops.RestoreDgraphFn)
	}
	entityRegistry["posts"] = swapped
	return ops
}

// waitForRun waits up to a minute for every post of the run to be live in
// Postgres, deleted of them still in the graph, and the run in status.
func waitForRun(t *testing.T, env *integration.Env, dg *integration.DgraphEnv, job uuid.UUID, ids []string, status string, deleted int) {
	t.Helper()
	var pgLive, graphDeleted int
	var got string
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, pq.Array(ids)).Scan(&pgLive); err != nil {
			t.Fatal(err)
		}
		if err := env.PG.QueryRow(`SELECT status FROM archive_jobs WHERE id = $1`, job).Scan(&got); err != nil {
			t.Fatal(err)
		}
		graphDeleted = deletedInGraph(t, dg, ids)
		if pgLive == len(ids) && graphDeleted == deleted && got == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a minute after the undo: %d of %d posts live in Postgres, %d still deleted in the graph, the run %q; want %d, %d and %q", pgLive, len(ids), graphDeleted, got, len(ids), deleted, status)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// deletedInGraph is how many of these posts the graph holds as deleted (a
// deletion time after 1970: a live post's is unset or the zero time).
func deletedInGraph(t *testing.T, dg *integration.DgraphEnv, ids []string) int {
	t.Helper()
	quoted, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	raw := dg.Query(t, fmt.Sprintf(`{ q(func: eq(post_uuid, %s)) @filter(gt(post_deleted_at, "1970-01-01T00:00:00Z")) { n: count(uid) } }`, strings.TrimSpace(string(quoted))))
	var out struct {
		Q []struct {
			N int `json:"n"`
		} `json:"q"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("read the graph's answer %s: %v", raw, err)
	}
	if len(out.Q) == 0 {
		return 0
	}
	return out.Q[0].N
}
