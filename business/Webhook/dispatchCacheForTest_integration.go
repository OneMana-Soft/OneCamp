//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// Every integration test starts with the outgoing webhooks' dispatch cache
// forgotten (tests/integration/processcache), as a new process would.
func init() {
	processcache.Register("business/Webhook: the outgoing webhooks each event goes to", ForgetDispatchCacheForTest)
}

// ForgetDispatchCacheForTest makes the process read again which outgoing
// webhooks each event goes to, after an integration test starts fresh
// databases. It exists only in the integration build: the tag keeps it out of
// the shipped binary, so no product code can reach it.
//
// The dispatch cache (webhookDispatchCache.go) keeps that list for 30 s,
// process-wide, and only the webhooks' own writes forget it. Tests write their
// webhooks straight to the table, so the list an earlier test's databases gave
// can answer for the next test's: every calls test's meeting call looks up
// call.started and call.ended at workspace scope, under one key, and the
// empty list stored there would hide the webhooks a test after it writes.
func ForgetDispatchCacheForTest() {
	bumpDispatchCacheVersion()
}
