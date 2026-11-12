//go:build integration
// +build integration

package integration_test

// The comment-owner repair on the graph (NOTIF-H2 backfill): old comments on
// Olu's "Press kit", each owned by its writer as every task comment was
// before 6 Nov 2026, are re-owned to Olu once, batched, and a second pass
// changes nothing. It also times the repair at a size above the demo's (the
// demo seeds no task comments; its visitors add a few before each reset).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOldTaskCommentsAreReownedToWhomTheyAreAddressed' -v

import (
	"context"
	"testing"
	"time"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// oldTaskComments is how many writer-owned comments the test lays down.
const oldTaskComments = 1200

func TestOldTaskCommentsAreReownedToWhomTheyAreAddressed(t *testing.T) {
	w := newTaskWorld(t)
	ctx := context.Background()
	if w.uids["kit"] == "" || w.uids["olu"] == "" {
		t.Fatalf("the world names no kit or Olu: %v", w.uids)
	}
	writers := []string{"ben", "tom", "maya"}
	nodes := make([]map[string]any, 0, oldTaskComments)
	at := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < oldTaskComments; i++ {
		by := w.uids[writers[i%len(writers)]]
		nodes = append(nodes, map[string]any{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "<p>Old</p>",
			"comment_by": map[string]string{"uid": by}, "comment_on_content_added_by": map[string]string{"uid": by},
			"comment_task": map[string]string{"uid": w.uids["kit"]}, "comment_created_at": at.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			"comment_deleted_at": "1970-01-01T00:00:00Z"})
	}
	if _, err := dgexec.Set(ctx, nodes); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rep, ran, err := taskBusiness.CommentOwnerRepair.Run(ctx)
	took := time.Since(start)
	if err != nil || !ran {
		t.Fatalf("the repair: ran %v, err %v", ran, err)
	}
	t.Logf("TIMING comment-owner repair over %d old task comments: %v (%s)", oldTaskComments, took, rep)
	// The graph is the run's: other tests' old comments may be there too.
	if rep.DryRun < oldTaskComments || rep.Reowned != rep.DryRun {
		t.Errorf("report %s; want a dry run of at least %d, all of them re-owned", rep, oldTaskComments)
	}
	if most := (rep.Read + 499) / 500; rep.Writes > most {
		t.Errorf("%d writes for %d comments read; want at most one a page of 500 (%d)", rep.Writes, rep.Read, most)
	}
	owned := ownersOnTask(t, w.uids["kit"])
	if owned[w.uids["olu"]] != oldTaskComments {
		t.Errorf("owners of the kit's comments %v; want Olu (%s) to own all %d old ones", owned, w.uids["olu"], oldTaskComments)
	}

	// A second pass (as a fresh install would run it) changes nothing.
	again := taskBusiness.CommentOwnerRepair
	again.Key += ".again"
	rep2, _, err := again.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.DryRun != 0 || rep2.Reowned != 0 || rep2.Writes != 0 {
		t.Errorf("second pass %s; want nothing counted or written", rep2)
	}
	if after := ownersOnTask(t, w.uids["kit"]); after[w.uids["olu"]] != oldTaskComments {
		t.Errorf("after the second pass Olu owns %d; want %d", after[w.uids["olu"]], oldTaskComments)
	}
}

// ownersOnTask counts the task's comments by their owner's uid.
func ownersOnTask(t *testing.T, taskUID string) map[string]int {
	t.Helper()
	type row struct {
		Owner struct {
			Uid string `json:"uid"`
		} `json:"comment_on_content_added_by"`
	}
	rows, err := dgexec.List[row](context.Background(), `{
		rows(func: has(comment_task)) @filter(uid_in(comment_task, `+taskUID+`)) { comment_on_content_added_by { uid } }
	}`, nil, "rows")
	if err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, r := range rows {
		n[r.Owner.Uid]++
	}
	return n
}
