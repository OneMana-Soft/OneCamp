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
