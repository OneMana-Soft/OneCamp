package business

// The drill is a setup step, because the guarantee is the reason to buy.
//
// A new admin's checklist walked them through a channel, their team, email and a
// model provider, and then stopped. Nothing in it asked them to see the one thing
// the product is sold on: an agent refused, on the record, before it acted. A
// customer could finish setting up the governed-AI workspace and never once watch
// it govern anything, which is the same failure the public demo had before the
// drill was staged there, now on the install they paid for.
//
// Registered from here rather than written into the onboarding package, because
// that package is compiled into the AI-free edition and must not import this
// one. Linking the AI packages is what puts the step on the list.

import (
	"context"

	onboarding "github.com/akashc777/OneCamp/business/Onboarding"
	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// OnboardingStepID is the step's id, exported for the test that checks it is on
// the list.
const OnboardingStepID = "drill"

// OnboardingStepWeight places the drill after the provider step (weight 10).
const OnboardingStepWeight = 20

// drillStep is what gets registered. Named so the test can read the definition
// without a registry accessor that nothing in production would call.
var drillStep = onboarding.Step{
	ID:     OnboardingStepID,
	Title:  "Watch an agent be refused",
	Detail: "One click. An agent tries a channel you are not in, is stopped before it posts, and the refusal lands in the audit chain.",
	Href:   "/app/admin?tab=ai-models",
}

func init() {
	// After the provider step. The drill needs no model, but a person reads the
	// list top to bottom, and "prove the guarantee" before "plug in the thing it
	// governs" reads as a list nobody thought about.
	onboarding.Register(drillStep, OnboardingStepWeight, drillStepIncluded, drillHasRefused)
}

// drillStepIncluded puts the step on the list whenever this build has AI at all,
// configured or not. The drill needs no model, so a fresh install with no
// provider yet can still run it; and on the edition with no AI the card the
// step links to does not exist, so the step must not either.
func drillStepIncluded(features map[string]bool) bool {
	_, present := features[helpers.FeatureNameAI]
	return present
}

// drillHasRefused reports whether this workspace has ever recorded a drill
// refusal. Any actor, deliberately: the step is about the workspace having seen
// the guarantee hold, not about which admin pressed the button.
//
// One row is enough and one row is all it asks for. "Not refused" rows do not
// count: a drill whose forbidden post went through is a broken install, and a
// checklist that marked that as done would be reading the wrong sign.
func drillHasRefused(ctx context.Context, _ userModels.UserInfo) bool {
	entries, err := auditModel.ListByActionPrefixes(ctx, []string{auditActionRefused}, nil, 1)
	return err == nil && len(entries) > 0
}
