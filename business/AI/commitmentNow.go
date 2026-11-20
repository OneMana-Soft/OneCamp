package business

import (
	"time"

	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
)

// When a remembered commitment needs its person now (V-581): the one answer
// the home's attention list and the nudge engine both use. An open question
// is nudged of by the same shape (V-615): only while recently stale.
//
// A commitment needs them now while it is open, dated, due before today and
// overdue for at most NeedsMeNowDays. One without a date is pending,
// never overdue. One overdue for longer is stale: it is never in "needs me
// now" nor an urgent nudge, and the memory page asks of it "Still relevant?".
// Live is the caller's read: every read of memory items leaves out the
// deleted ones (deleted_at IS NULL).

// NeedsMeNowDays is how many days overdue a commitment, or stale an open
// question, may be and still need its person now. The demo seeds a commitment
// past it (business/AIAgent/demoagent). It is shorter than the 30 days a
// handled nudge is kept (nudgePurgeAfter), so a dismissal outlives every
// signal it holds.
const NeedsMeNowDays = 14

// daysBefore is how many days t's day is before now's day, counted in now's
// time zone: 0 for the same day, negative for a later one.
func daysBefore(t, now time.Time) int {
	day := func(t time.Time) time.Time {
		y, m, d := t.In(now.Location()).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return int(day(now).Sub(day(t)).Hours() / 24)
}

// commitmentOverdueDays is how many days before now's day an open, dated
// commitment was due: 0 for due today, negative for due later. ok is false
// for anything else. Days are counted in now's time zone.
func commitmentOverdueDays(it *memoryModels.MemoryItem, now time.Time) (days int, ok bool) {
	if it == nil || it.Kind != memoryModels.KindCommitment || it.Status != memoryModels.StatusOpen || it.DueAt == nil {
		return 0, false
	}
	return daysBefore(*it.DueAt, now), true
}

// commitmentNeedsMeNow: open, dated, due before today and overdue for at most
// NeedsMeNowDays.
func commitmentNeedsMeNow(it *memoryModels.MemoryItem, now time.Time) bool {
	days, ok := commitmentOverdueDays(it, now)
	return ok && days >= 1 && days <= NeedsMeNowDays
}

// commitmentIsStale: open, dated, and overdue for longer than
// NeedsMeNowDays.
func commitmentIsStale(it *memoryModels.MemoryItem, now time.Time) bool {
	days, ok := commitmentOverdueDays(it, now)
	return ok && days > NeedsMeNowDays
}

// questionNeedsMeNow: an open question gone stale (untouched for
// nudgeStaleAfter) at most NeedsMeNowDays ago. One stale for longer is never
// nudged of, so a dismissal (kept nudgePurgeAfter) outlives its window.
func questionNeedsMeNow(it *memoryModels.MemoryItem, now time.Time) bool {
	if it == nil || it.Kind != memoryModels.KindQuestion || it.Status != memoryModels.StatusOpen {
		return false
	}
	staleSince := it.UpdatedAt.Add(nudgeStaleAfter)
	return !staleSince.After(now) && daysBefore(staleSince, now) <= NeedsMeNowDays
}
