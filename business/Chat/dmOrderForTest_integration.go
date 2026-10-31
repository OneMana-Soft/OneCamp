//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// Whether the DM order's backfill has finished is kept for the process
// (dmList.go dmOrderReady). In the integration build it is forgotten as every
// test starts: each test has databases of its own, and a test that started
// the sweep jobs (chat.dm-order runs at start, finds nothing to order in its
// empty databases and says it finished) made the next test's DM list read in
// pages before that test's own backfill had run (V-28: TestTheDMListInPages
// after TestCalendarImportOfAGoogleExportOverAJob).
func init() {
	processcache.Register("business/Chat: the DM order's backfill, seen finished", ForgetDMOrderForTest)
}

// ForgetDMOrderForTest forgets that this process saw the backfill finish.
func ForgetDMOrderForTest() {
	dmOrderReady.Store(false)
}
