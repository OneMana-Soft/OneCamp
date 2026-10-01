package business

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// TestTheDrillIsRegisteredAsASetupStep pins the registration itself. The step is
// contributed from init, so the way it disappears on the AI-free edition is that
// this package is not linked; on a build that links it, it must be on the list
// with somewhere to go.
func TestTheDrillIsRegisteredAsASetupStep(t *testing.T) {
	if drillStep.ID != OnboardingStepID || drillStep.Title == "" || drillStep.Detail == "" {
		t.Error("the drill step is missing its id, title or detail")
	}
	// The card lives on the AI tab of the admin page. A link to the default tab
	// would leave a new admin scrolling the wrong page for a card that is not
	// there.
	if drillStep.Href != "/app/admin?tab=ai-models" {
		t.Errorf("drill step points at %q, the card is on the ai-models tab", drillStep.Href)
	}
}

// TestTheDrillStepShowsWheneverAIIsPresent covers the include rule. Present but
// off is the state every fresh AI install is in, and the drill works there
// because it needs no model, so the step must show. Absent is the AI-free
// edition, where the card does not exist.
func TestTheDrillStepShowsWheneverAIIsPresent(t *testing.T) {
	if !drillStepIncluded(map[string]bool{helpers.FeatureNameAI: false}) {
		t.Error("the drill step hid itself on a build with AI but no provider")
	}
	if !drillStepIncluded(map[string]bool{helpers.FeatureNameAI: true}) {
		t.Error("the drill step hid itself on a configured AI build")
	}
	if drillStepIncluded(map[string]bool{}) {
		t.Error("the drill step appeared with no AI feature registered at all")
	}
}
