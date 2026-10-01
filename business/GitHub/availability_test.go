package business

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// Linking this package must announce the feature, because that is the entire
// mechanism: the task panel reads FEATURE_GITHUB from the client config instead
// of calling /admin/github/status, which 403s for every non-admin. If the
// registration is lost the flag silently reports absent and the pull-request
// affordance disappears for everyone, admins included.
func TestGitHubAnnouncesItselfAsAFeature(t *testing.T) {
	if !helpers.FeatureRegistered(helpers.FeatureNameGitHub) {
		t.Fatalf("linking business/GitHub did not register %q", helpers.FeatureNameGitHub)
	}
}

// The name is a wire contract: the frontend gates on this exact string, and a
// typo on either side fails by hiding the feature, which is the least visible
// kind of wrong.
func TestFeatureNameMatchesTheFrontendConstant(t *testing.T) {
	if helpers.FeatureNameGitHub != "github" {
		t.Errorf("FeatureNameGitHub = %q, frontend expects \"github\"", helpers.FeatureNameGitHub)
	}
}
