//go:build integration

package business

import "github.com/akashc777/OneCamp/tests/integration/processcache"

// The workspace settings are cached for the process for up to settingsTTL
// (settingsBusiness.go). In the integration build they are forgotten as every
// test starts (tests/integration/processcache), so the choices of the test
// before (the channels new members join, say) don't apply in the next.
func init() {
	processcache.Register("business/Settings: the workspace settings", ForgetSettingsForTest)
}
