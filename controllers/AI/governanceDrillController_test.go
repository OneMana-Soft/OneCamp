package controllers

import (
	"net/http"
	"testing"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	drillBusiness "github.com/akashc777/OneCamp/business/AIDrill"
	"github.com/akashc777/OneCamp/helpers"
)

func TestAMemberIsToldWhoCanSetTheDrillUp(t *testing.T) {
	status, msg, _ := memberDrillGate(false, helpers.NewRateLimiter(5, time.Minute), "someone")
	if status != http.StatusConflict {
		t.Errorf("an unseeded workspace answered %d, wanted %d", status, http.StatusConflict)
	}
	if msg == "" {
		t.Error("refused with no explanation of who can fix it")
	}
}

// Five clicks on a button that was never going to work must not lock the person
// out of the one that would, once an admin sets the drill up.
func TestAnUnseededWorkspaceDoesNotSpendTheAllowance(t *testing.T) {
	limiter := helpers.NewRateLimiter(2, time.Minute)
	for i := 0; i < 5; i++ {
		memberDrillGate(false, limiter, "someone")
	}
	if status, _, _ := memberDrillGate(true, limiter, "someone"); status != 0 {
		t.Errorf("the first real attempt was refused with %d; the failed ones were counted", status)
	}
}

func TestRepeatedRunsAreBounded(t *testing.T) {
	limiter := helpers.NewRateLimiter(2, time.Minute)
	for i := 1; i <= 2; i++ {
		if status, _, _ := memberDrillGate(true, limiter, "someone"); status != 0 {
			t.Fatalf("run %d was refused with %d", i, status)
		}
	}
	status, msg, wait := memberDrillGate(true, limiter, "someone")
	if status != http.StatusTooManyRequests {
		t.Fatalf("a third run answered %d, wanted %d", status, http.StatusTooManyRequests)
	}
	if msg == "" {
		t.Error("refused with no explanation")
	}
	if wait <= 0 {
		t.Error("told the caller to come back later without saying when")
	}
}

func TestOnePersonsRunsDoNotBlockAnothers(t *testing.T) {
	limiter := helpers.NewRateLimiter(1, time.Minute)
	memberDrillGate(true, limiter, "first")
	if status, _, _ := memberDrillGate(true, limiter, "second"); status != 0 {
		t.Errorf("a second person was refused with %d because the first had run it", status)
	}
}

// TestTheChecklistConnectsAProviderBeforeItRunsTheDrill pins the order the two
// AI setup steps appear in. Both are contributed from package init and ordered
// by the weight each declares; this is the first package that links both, so
// this is where the two weights can be compared.
//
// Order is content: connect a provider, then watch an agent be refused. The
// drill needs no model, but a person reads the list top to bottom, and "prove
// the guarantee" before "plug in the thing it governs" reads as a list that was
// not thought about.
func TestTheChecklistConnectsAProviderBeforeItRunsTheDrill(t *testing.T) {
	if aiBusiness.ProviderStepWeight >= drillBusiness.OnboardingStepWeight {
		t.Errorf("provider step weight %d must be below the drill's %d",
			aiBusiness.ProviderStepWeight, drillBusiness.OnboardingStepWeight)
	}
}
