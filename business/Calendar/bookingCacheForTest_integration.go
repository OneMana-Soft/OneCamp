//go:build integration

package Calendar

import "github.com/akashc777/OneCamp/tests/integration/processcache"

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
