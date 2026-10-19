//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The automation bot's identity is kept for the process (automationBot.go).
// In the integration build it is forgotten as every test starts
// (tests/integration/processcache). Each test has databases of its own, and a
// bot resolved by the test before would post as a user they don't have.
func init() {
	processcache.Register("business/User: the automation bot", ForgetAutomationBotForTest)
}
