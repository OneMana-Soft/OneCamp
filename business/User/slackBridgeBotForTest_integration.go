//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The Slack bridge's principal is kept for the process (agentBot.go
// EnsureSlackBridgeBot). In the integration build it is forgotten as every
// test starts (tests/integration/processcache), as the guests' principal and
// the automation bot are: each test has databases of its own, and a principal
// resolved by the test before would post as a user they don't have (the
// bridge's inbound message never became a post after a test that linked a
// channel).
func init() {
	processcache.Register("business/User: the Slack bridge's principal", ForgetSlackBridgeBotForTest)
}

// ForgetSlackBridgeBotForTest drops the kept Slack bridge principal.
func ForgetSlackBridgeBotForTest() {
	slackBridgeBotMu.Lock()
	defer slackBridgeBotMu.Unlock()
	slackBridgeBot = nil
}
