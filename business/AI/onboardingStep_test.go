package business

import (
	"context"
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// TestTheProviderStepIsRegisteredFromHere pins where the step lives. It moved
// out of the onboarding package so that package carries no AI knowledge, and so
// that the done-probe can dial the provider, which the onboarding package could
// not do without importing this one.
func TestTheProviderStepIsRegisteredFromHere(t *testing.T) {
	if providerStep.ID != "ai" {
		t.Errorf("the client keys on the id; it must stay %q, got %q", "ai", providerStep.ID)
	}
	if providerStep.Href != "/app/admin?tab=ai-models" {
		t.Errorf("provider step points at %q; the provider card is on the ai-models tab", providerStep.Href)
	}
	if providerStep.Title == "" || providerStep.Detail == "" {
		t.Error("the provider step has no title or detail")
	}
}

// TestProviderProbeIsCached guards the cost. The home page asks for the
// checklist on every load; without the cache each load would dial the provider
// and, with it down, wait the full timeout.
func TestProviderProbeIsCached(t *testing.T) {
	ForgetProviderProbe()
	providerReachable.Set(providerProbeKey, true)
	if !ActiveChatProviderAnswers(context.Background()) {
		t.Fatal("a cached true was not returned")
	}
	providerReachable.Set(providerProbeKey, false)
	if ActiveChatProviderAnswers(context.Background()) {
		t.Fatal("a cached false was not returned; negative results must be cached too")
	}
	ForgetProviderProbe()
}

// TestReloadForgetsTheProbe is the link between fixing the provider and the
// checklist noticing. An admin who saves a working key and sees the step still
// unticked reads that as the save not having worked.
func TestReloadForgetsTheProbe(t *testing.T) {
	providerReachable.Set(providerProbeKey, false)
	ForgetProviderProbe()
	if _, ok := providerReachable.Get(providerProbeKey); ok {
		t.Fatal("the probe result survived ForgetProviderProbe")
	}
}

// TestNothingSelectedIsNotConnected covers the half of the probe that needs no
// network. AI switched off, or no chat provider chosen, must read as "not
// connected" before anything is dialled.
func TestNothingSelectedIsNotConnected(t *testing.T) {
	id := uuid.New()
	for name, s := range map[string]*aiModels.AISettings{
		"no settings row": nil,
		"ai switched off": {Enabled: false, ChatProviderID: &id},
		"no provider":     {Enabled: true, ChatProviderID: nil},
	} {
		if providerSelected(s) {
			t.Errorf("%s: read as a selected provider", name)
		}
	}
	if !providerSelected(&aiModels.AISettings{Enabled: true, ChatProviderID: &id}) {
		t.Error("an enabled, chosen provider should be worth dialling")
	}
}
