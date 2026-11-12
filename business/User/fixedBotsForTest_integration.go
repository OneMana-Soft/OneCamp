//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The fixed principals are kept for the process (agentBot.go fixedBot). In the
// integration build they are forgotten as every test starts
// (tests/integration/processcache), as the automation bot is: each test has
// databases of its own, and a principal resolved by the test before would
// write as a user they don't have (a guest's channel message failed
// posts_created_by_fkey after a test that made one, and the Slack bridge's
// inbound message never became a post after a test that linked a channel).
// A code host's bot (EnsureIntegrationBot) is one of them: a tag's release
// failed releases_created_by_fkey after a test that made GitHub's bot.
func init() {
	processcache.Register("business/User: the fixed principals", ForgetFixedBotsForTest)
}

// ForgetFixedBotsForTest drops every kept fixed principal, each code host's
// included.
func ForgetFixedBotsForTest() {
	for _, b := range []*fixedBot{slackBridgeBot, channelGuestBot, checkInBot, upgradeRepairBot} {
		b.mu.Lock()
		b.id = nil
		b.mu.Unlock()
	}
	integrationBotsMu.Lock()
	clear(integrationBots)
	integrationBotsMu.Unlock()
}
