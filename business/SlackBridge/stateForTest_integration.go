//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The bridge's configuration is cached for the process for up to stateTTL
// (bridge.go). In the integration build it is forgotten as every test starts
// (tests/integration/processcache), so a link the test before made isn't read
// against databases that never held it.
func init() {
	processcache.Register("business/SlackBridge: the bridge's configuration", ForgetStateForTest)
}
