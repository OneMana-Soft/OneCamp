package business

import (
	"time"

	recurrenceModel "github.com/akashc777/OneCamp/models/postgres/TaskRecurrence"
)

// maxDueAheadSteps bounds the walk from a task's due date to the stretch a
// calendar shows: a daily repeat read a few years ahead.
const maxDueAheadSteps = 1500

// DueAhead is when a task that repeats on a schedule will be due next, from
// its due date on, each time done on time, from `from` until before `to`, at most
// max dates: the dates completing it will give the next ones (NextOccurrence),
// so a calendar can show them before they exist (calendar head 9, gcal-1b).
// The task as it is now is not among them. One overdue counts from now, as
// one done late skips the dates already gone. A repeat after completion, or a
// task with no due date, has none. Pure.
func DueAhead(r recurrenceModel.TaskRecurrence, due *time.Time, now, from, to time.Time, max int) []time.Time {
	if r.Mode != recurrenceModel.ModeSchedule || !isSet(due) || max <= 0 || !to.After(from) {
		return nil
	}
	var out []time.Time
	cur := *due
	done := now
	if cur.After(done) {
		done = cur
	}
	for i := 0; i < maxDueAheadSteps && len(out) < max; i++ {
		next, _, anchor := NextOccurrence(r, &cur, nil, done)
		if next.IsZero() || !next.After(cur) || !next.Before(to) {
			break
		}
		if !next.Before(from) {
			out = append(out, next)
		}
		r.AnchorDay = anchor
		cur, done = next, next
	}
	return out
}
