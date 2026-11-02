package business

import (
	"context"
	"errors"
	"testing"
	"time"

	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
)

// pruneLog stands in for task_changes' two prune statements: old changes
// numbered before keepFrom, deleted oldest first.
type pruneLog struct {
	keepFrom int64
	old      int64
	cutoffs  []time.Time
	deletes  int
	failAt   int
	failKeep error
}

func swapPrune(t *testing.T, p *pruneLog) {
	t.Helper()
	f, d := firstToKeep, deleteBefore
	t.Cleanup(func() { firstToKeep, deleteBefore = f, d })
	firstToKeep = func(_ context.Context, cutoff time.Time) (int64, error) {
		p.cutoffs = append(p.cutoffs, cutoff)
		return p.keepFrom, p.failKeep
	}
	deleteBefore = func(_ context.Context, seq int64, limit int) (int64, error) {
		p.deletes++
		if seq != p.keepFrom || limit != pruneBatch {
			t.Errorf("deleted before %d, %d at a time; want before %d, %d at a time", seq, limit, p.keepFrom, pruneBatch)
		}
		if p.deletes == p.failAt {
			return 0, errors.New("the database went away")
		}
		n := min(p.old, int64(limit))
		p.old -= n
		return n, nil
	}
}

// The prune deletes every change before the first it keeps, a batch a
// statement, oldest first, and stops when a batch comes back short.
func TestPruneDeletesTheOldEndInBatches(t *testing.T) {
	p := &pruneLog{keepFrom: 12_001, old: 12_000}
	swapPrune(t, p)
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	gone, err := prune(context.Background(), cutoff)
	if err != nil || gone != 12_000 || p.old != 0 || p.deletes != 3 {
		t.Fatalf("gone %d in %d deletes (err %v), %d left; want 12000 in 3", gone, p.deletes, err, p.old)
	}
	if len(p.cutoffs) != 1 || !p.cutoffs[0].Equal(cutoff) {
		t.Errorf("asked what to keep from %v, want once from %v", p.cutoffs, cutoff)
	}
}

// Nothing before the first change kept (or an empty log) deletes nothing; a
// read or a delete that fails ends the pass with its error and what went.
func TestPruneStopsWhenThereIsNothingOrAnError(t *testing.T) {
	for _, keep := range []int64{0, 1} {
		p := &pruneLog{keepFrom: keep}
		swapPrune(t, p)
		if gone, err := prune(context.Background(), time.Now()); err != nil || gone != 0 || p.deletes != 0 {
			t.Errorf("keep from %d: gone %d in %d deletes, err %v", keep, gone, p.deletes, err)
		}
	}
	p := &pruneLog{keepFrom: 9, failKeep: errors.New("no")}
	swapPrune(t, p)
	if _, err := prune(context.Background(), time.Now()); err == nil || p.deletes != 0 {
		t.Errorf("a failed read: err %v, %d deletes", err, p.deletes)
	}
	p = &pruneLog{keepFrom: 20_001, old: 20_000, failAt: 2}
	swapPrune(t, p)
	if gone, err := prune(context.Background(), time.Now()); err == nil || gone != pruneBatch || p.deletes != 2 {
		t.Errorf("a failed delete: gone %d in %d deletes, err %v; want %d and the error", gone, p.deletes, err, pruneBatch)
	}
}

// A pass deletes at most pruneMaxBatches batches; the next pass goes on.
func TestAPruneIsBoundedPerPass(t *testing.T) {
	p := &pruneLog{keepFrom: 1 << 40, old: 1 << 39}
	swapPrune(t, p)
	if gone, err := prune(context.Background(), time.Now()); err != nil || p.deletes != pruneMaxBatches || gone != pruneMaxBatches*pruneBatch {
		t.Errorf("gone %d in %d deletes (err %v), want %d deletes", gone, p.deletes, err, pruneMaxBatches)
	}
}

// The projects programme's sweep runs the prune with a cutoff keptFor ago.
func TestTheSweepPrunesWhatIsOlderThanTheLogKeeps(t *testing.T) {
	p := &pruneLog{keepFrom: 3, old: 2}
	swapPrune(t, p)
	if err := workSweep.WorkChangesPrune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.cutoffs) != 1 || p.old != 0 {
		t.Fatalf("cutoffs %v, %d old left: the sweep didn't prune", p.cutoffs, p.old)
	}
	if ago := time.Since(p.cutoffs[0]); ago < keptFor-time.Minute || ago > keptFor+time.Minute {
		t.Errorf("cutoff %v ago, want %v", ago, keptFor)
	}
}
