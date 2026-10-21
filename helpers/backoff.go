package helpers

import (
	"math/rand/v2"
	"time"
)

// Backoff is the pause before retry number attempt (1, 2, 3…): base, doubling
// each time up to max, plus up to a quarter more at random, so callers that
// failed together don't all come back at the same moment.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := max
	if shift := uint(attempt - 1); base <= max>>shift {
		d = base << shift
	}
	if j := int64(d / 4); j > 0 {
		d += time.Duration(rand.Int64N(j))
	}
	return min(d, max)
}

// ScheduleWait is the pause before try number attempt (1, 2, 3…) on a fixed
// schedule: the schedule's entry for it, its last for any try past the end,
// plus up to jitter of it more at random (0 for none, 0.25 for a quarter), so
// callers that failed together don't all come back at once. An empty
// schedule waits nothing. Each caller keeps its own table (business/Outbox,
// business/Hooks).
func ScheduleWait(schedule []time.Duration, attempt int, jitter float64) time.Duration {
	if len(schedule) == 0 {
		return 0
	}
	d := schedule[min(max(attempt, 1), len(schedule))-1]
	if j := int64(float64(d) * jitter); j > 0 {
		d += time.Duration(rand.Int64N(j + 1))
	}
	return d
}
