//go:build integration

package generation

import "time"

// HoldClockForTest stops the clock entries age by until release: an entry
// filled while it is held is served, at its generation, however long the
// test takes, as it would be within MaxAge. The store-budget harness holds it
// from a route's warm-up to its measured call, so a loaded machine can't
// expire what the warm-up filled. It exists only in the integration build,
// so no product code can reach it.
func HoldClockForTest() (release func()) {
	at := time.Now()
	heldAt.Store(&at)
	return func() { heldAt.Store(nil) }
}

// AsAnotherProcessForTest runs write as if another process made it: this
// process's own count of committed writes is put back after it, so only the
// shared generation in Redis can tell this process's caches that the rows
// changed. Integration build only.
func AsAnotherProcessForTest(write func()) {
	at := bumps.Load()
	write()
	bumps.Store(at)
}
