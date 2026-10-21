package business_test

import (
	"testing"

	marketplaceBusiness "github.com/akashc777/OneCamp/business/Marketplace"
	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
)

// The sample's brief is made from OneCamp's own "Project brief" doc template,
// asked for by its fixed id: Onboarding can't import the marketplace (it
// imports the agents, which import Onboarding), so the id is written there and
// held to the built-in list here.
func TestTheSampleBriefIsTheProjectBriefTemplate(t *testing.T) {
	for _, tpl := range marketplaceBusiness.BuiltinDocTemplates {
		if tpl.ID.String() == onboardingBusiness.StartHereTemplateID {
			if tpl.Name != "Project brief" {
				t.Fatalf("the sample's brief is %q, not the Project brief", tpl.Name)
			}
			return
		}
	}
	t.Fatalf("%s is no built-in doc template", onboardingBusiness.StartHereTemplateID)
}
