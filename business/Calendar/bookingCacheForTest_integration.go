//go:build integration

package Calendar

import (
	"sync/atomic"

	domain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/tests/integration/processcache"
)

// Every integration test starts with the booking pages forgotten
// (tests/integration/processcache), as a new process would.
func init() {
	processcache.Register("business/Calendar: the booking pages", ForgetBookingPagesForTest)
}

// ForgetBookingPagesForTest drops every booking page, answer and busy time
// kept for the process, after an integration test starts fresh databases. It
// exists only in the integration build: the tag keeps it out of the shipped
// binary, so no product code can reach it.
//
// The booking pages' cache (bookingCache.go) keeps a page by its address for
// a minute, process-wide. The booking test runs once per calendar store in
// one process, each run with databases of its own, and the run before's
// "office-hours" answered for the next run's deleted page whenever the second
// run reached it within the minute. A new cache, as this package's own tests
// take one (withPage): a read still under way from the test before stores
// into the old cache, which nothing reads.
func ForgetBookingPagesForTest() {
	pageCache = newBookingPagesCache()
}

// WhileRescheduleHeldForTest runs f inside each guest's move's page lock,
// after its slot check and before the move, until the restore it returns is
// called: the lock test books the same time meanwhile. Integration build only.
func WhileRescheduleHeldForTest(f func()) (restore func()) {
	old := rescheduleHeld
	rescheduleHeld = f
	return func() { rescheduleHeld = old }
}

// WhileSlotCheckForTest runs f as each booking's or move's slot check starts,
// before the calendars are read, until the restore it returns is called: the
// lock test asks whether the page's lock is held then. Integration build only.
func WhileSlotCheckForTest(f func()) (restore func()) {
	old := slotChecking
	slotChecking = f
	return func() { slotChecking = old }
}

// CountEventReadsForTest counts every read of one event, from either store
// (the store's load, the graph's GetDgraphEventInfoByUUID), until the restore
// it returns is called: a guest's move reads its meeting once. Integration
// build only.
func CountEventReadsForTest() (reads func() int64, restore func()) {
	var n atomic.Int64
	count := func() { n.Add(1) }
	old := eventLoaded
	eventLoaded = count
	restoreGraph := domain.WhileEventReadForTest(count)
	return n.Load, func() {
		eventLoaded = old
		restoreGraph()
	}
}
