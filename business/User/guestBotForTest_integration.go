//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The guests' principal is kept for the process (agentBot.go EnsureGuestBot).
// In the integration build it is forgotten as every test starts, as the
// automation bot is: each test has databases of its own, and a principal
// resolved by the test before would write as a user it doesn't have (a guest's
// channel message failed posts_created_by_fkey after a test that made one).
func init() {
	processcache.Register("business/User: the guests' principal", ForgetGuestBotForTest)
}

// ForgetGuestBotForTest drops the kept guests' principal.
func ForgetGuestBotForTest() {
	channelGuestBotMu.Lock()
	defer channelGuestBotMu.Unlock()
	channelGuestBot = nil
}
