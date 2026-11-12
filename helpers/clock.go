package helpers

import (
	"sync/atomic"
	"time"
)

// clock is what Now answers instead of the real clock. Only the integration
// build sets it (clockForTest_integration.go, SetClockForTest); in the shipped
// binary it stays nil, and nothing a request carries (a header, a query, a
// cookie) or an environment variable reaches it.
var clock atomic.Pointer[func() time.Time]

// Now is the time now, as a handler reads it: the real clock (time.Now). It is
// the one seam an in-process test turns to ask a handler at an instant of its
// choosing, such as the last hour of a week; handlers pass it on to business
// functions that take now, rather than each package keeping a clock of its own.
func Now() time.Time {
	if f := clock.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}
