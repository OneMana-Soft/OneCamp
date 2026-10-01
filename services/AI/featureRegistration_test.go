package ai

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// TestLinkingThisPackageRegistersTheAIFeature is the other half of the edition mechanism,
// and it can only be asserted from inside this package.
//
// helpers/features_test.go proves that an unregistered feature is absent. This proves the
// registration actually happens when the package is linked — so on v2 the frontend is told
// AI exists, and on v1, which is built without this package, nothing tells it anything.
//
// Asserted rather than assumed because the whole mechanism hangs on a package init that no
// other code calls. Delete that init and every test here still passes except this one,
// while the frontend quietly stops offering AI on the edition that has it.
func TestLinkingThisPackageRegistersTheAIFeature(t *testing.T) {
	registered := helpers.RegisteredFeatures()

	found := false
	for _, name := range registered {
		if name == helpers.FeatureNameAI {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("importing services/AI did not register the %q feature (registered: %v). "+
			"/config/client reports the registry, so the frontend would hide every AI entry "+
			"point on an edition that has AI.", helpers.FeatureNameAI, registered)
	}
}

// TestAIFeatureProbeIsSafeBeforeInitialisation covers the window every server passes
// through: the package is linked and has registered, but no service has been built yet.
//
// GetService returns nil there. The probe must read that as "not available" rather than
// panicking, because /config/client is one of the first requests a loading page makes and
// a panic would be a 500 on the request that decides what to render.
func TestAIFeatureProbeIsSafeBeforeInitialisation(t *testing.T) {
	saved := servicePtr.Load()
	servicePtr.Store(nil)
	t.Cleanup(func() { servicePtr.Store(saved) })

	var status map[string]bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the AI probe panicked with no service configured: %v", r)
			}
		}()
		status = helpers.FeatureStatus()
	}()

	if status[helpers.FeatureNameAI] {
		t.Error("no AI service is configured, but the feature reports usable. The frontend " +
			"would offer AI entry points whose every call fails.")
	}
}
