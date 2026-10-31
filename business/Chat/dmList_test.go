package business

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	chatModels "github.com/akashc777/OneCamp/models/postgres/Chat"
)

// fakeDMOrder stands in for the backfill's store: left DMs to stamp, a done
// flag, and how many batches ran.
type fakeDMOrder struct {
	left, batches int
	done          bool
	fail          error
}

func (f *fakeDMOrder) install(t *testing.T) {
	t.Helper()
	stamp, isDone, mark := stampUnstampedDMs, dmOrderDone, markDMOrderDone
	dmOrderReady.Store(false)
	stampUnstampedDMs = func(_ context.Context, batch int) (int, error) {
		f.batches++
		if f.fail != nil {
			return 0, f.fail
		}
		n := min(batch, f.left)
		f.left -= n
		return n, nil
	}
	dmOrderDone = func() (bool, error) { return f.done, nil }
	markDMOrderDone = func(string) error { f.done = true; return nil }
	t.Cleanup(func() {
		stampUnstampedDMs, dmOrderDone, markDMOrderDone = stamp, isDone, mark
		dmOrderReady.Store(false)
	})
}

func TestTheDMOrderBackfillStampsInBatchesThenRecordsItIsDone(t *testing.T) {
	f := &fakeDMOrder{left: 3*dmOrderBatch + 1}
	f.install(t)
	if err := runDMOrder(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.left != 0 || !f.done || f.batches != 5 || !dmListPaged() {
		t.Fatalf("left %d, done %v, %d batches, paged %v; want 0, true, 5 (four with DMs, one finding none), true", f.left, f.done, f.batches, dmListPaged())
	}
	if err := runDMOrder(context.Background()); err != nil || f.batches != 5 {
		t.Fatalf("run again: %v, %d batches; want nothing done", err, f.batches)
	}
}

func TestTheDMOrderBackfillCarriesOnNextRunWhenOneRunIsntEnough(t *testing.T) {
	f := &fakeDMOrder{left: (dmOrderBatchesPerRun + 1) * dmOrderBatch}
	f.install(t)
	if err := runDMOrder(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.done || dmListPaged() || f.left != dmOrderBatch {
		t.Fatalf("after one run: done %v, paged %v, %d left; want false, false, %d", f.done, dmListPaged(), f.left, dmOrderBatch)
	}
	if err := runDMOrder(context.Background()); err != nil || !f.done || !dmListPaged() {
		t.Fatalf("after the next run: %v, done %v, paged %v", err, f.done, dmListPaged())
	}
}

func TestTheDMOrderBackfillStopsAtAFailureAndIsNotDone(t *testing.T) {
	f := &fakeDMOrder{left: 10, fail: errors.New("graph down")}
	f.install(t)
	if err := runDMOrder(context.Background()); err == nil || f.done || dmListPaged() {
		t.Fatalf("a failing batch: %v, done %v, paged %v; want the error, not done", err, f.done, dmListPaged())
	}
}

func TestAFirstPageBringsTheMostRecentlyActiveUnreadDMs(t *testing.T) {
	base := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	counts := map[string]*chatModels.ChatMessageCount{
		"read": {GrpId: "read", ChatCount: 0, LastUnreadAt: sql.NullTime{}},
	}
	for i := range maxUnreadOffPage + 2 {
		id := fmt.Sprintf("g%03d", i)
		counts[id] = &chatModels.ChatMessageCount{GrpId: id, ChatCount: 1, LastUnreadAt: sql.NullTime{Time: base.Add(time.Duration(i) * time.Minute), Valid: true}}
	}
	ids := unreadIDs(counts)
	if len(ids) != maxUnreadOffPage {
		t.Fatalf("%d ids, want the cap %d", len(ids), maxUnreadOffPage)
	}
	newest := fmt.Sprintf("g%03d", maxUnreadOffPage+1)
	if ids[0] != newest || slices.Contains(ids, "g000") || slices.Contains(ids, "g001") || slices.Contains(ids, "read") {
		t.Fatalf("first %s, want %s; the two oldest and the read one must be left out: %v", ids[0], newest, ids[len(ids)-3:])
	}
}
