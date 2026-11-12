package business

import (
	"time"

	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
)

// When a remembered commitment needs its person now (V-581): the one answer
// the home's attention list and the nudge engine both use.
//
// A commitment needs them now while it is open, dated, due before today and
// overdue for at most CommitmentStaleAfterDays. One without a date is pending,
// never overdue. One overdue for longer is stale: it is never in "needs me
// now" nor an urgent nudge, and the memory page asks of it "Still relevant?".
// Live is the caller's read: every read of memory items leaves out the
// deleted ones (deleted_at IS NULL).

// CommitmentStaleAfterDays is how many days overdue a commitment may be and
// still need its person now. The demo seeds one past it (business/AIAgent/demoagent).
const CommitmentStaleAfterDays = 14

// commitmentOverdueDays is how many days before now's day an open, dated
// commitment was due: 0 for due today, negative for due later. ok is false
// for anything else. Days are counted in now's time zone.
func commitmentOverdueDays(it *memoryModels.MemoryItem, now time.Time) (days int, ok bool) {
	if it == nil || it.Kind != memoryModels.KindCommitment || it.Status != memoryModels.StatusOpen || it.DueAt == nil {
		return 0, false
	}
	day := func(t time.Time) time.Time {
		y, m, d := t.In(now.Location()).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return int(day(now).Sub(day(*it.DueAt)).Hours() / 24), true
}

// commitmentNeedsMeNow: open, dated, due before today and overdue for at most
// CommitmentStaleAfterDays.
func commitmentNeedsMeNow(it *memoryModels.MemoryItem, now time.Time) bool {
	days, ok := commitmentOverdueDays(it, now)
	return ok && days >= 1 && days <= CommitmentStaleAfterDays
}

// commitmentIsStale: open, dated, and overdue for longer than
// CommitmentStaleAfterDays.
func commitmentIsStale(it *memoryModels.MemoryItem, now time.Time) bool {
	days, ok := commitmentOverdueDays(it, now)
	return ok && days > CommitmentStaleAfterDays
}
