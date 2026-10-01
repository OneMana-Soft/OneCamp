package helpers

import (
	"sync"
	"testing"
)

// withCleanRegistry swaps in an empty registry for one test and restores it after, so
// these tests neither see nor disturb the real registrations made by package init.
func withCleanRegistry(t *testing.T) {
	t.Helper()

	featureMu.Lock()
	saved := featureProbes
	featureProbes = map[string]func() bool{}
	featureMu.Unlock()

	t.Cleanup(func() {
		featureMu.Lock()
		featureProbes = saved
		featureMu.Unlock()
	})
}

// TestUnregisteredFeatureIsAbsentNotFalse is the property the AI-free edition depends on.
//
// v1 is built without the AI packages, so nothing registers "ai". The client must be able
// to tell that apart from "present but switched off" — not because it renders differently
// today, but because ABSENT is a fact about the build and FALSE is a fact about the
// configuration, and an operator asking "why is there no AI" needs the difference.
func TestUnregisteredFeatureIsAbsentNotFalse(t *testing.T) {
	withCleanRegistry(t)

	status := FeatureStatus()
	if _, present := status[FeatureNameAI]; present {
		t.Errorf("nothing registered %q yet it appears in the status map as %v; an edition "+
			"built without the subsystem must report no key at all",
			FeatureNameAI, status[FeatureNameAI])
	}

	RegisterFeature(FeatureNameAI, func() bool { return false })
	status = FeatureStatus()
	usable, present := status[FeatureNameAI]
	if !present {
		t.Fatalf("%q was registered but is missing from the status map", FeatureNameAI)
	}
	if usable {
		t.Errorf("%q probe returns false but status says usable", FeatureNameAI)
	}
}

// TestFeatureProbeIsEvaluatedPerCall covers runtime reconfiguration: an admin can switch
// AI on or off, and a value captured at registration would be stale from then on.
func TestFeatureProbeIsEvaluatedPerCall(t *testing.T) {
	withCleanRegistry(t)

	enabled := false
	RegisterFeature("switchable", func() bool { return enabled })

	if FeatureStatus()["switchable"] {
		t.Error("probe returned false but status said usable")
	}

	enabled = true
	if !FeatureStatus()["switchable"] {
		t.Error("the probe now returns true, but status still says unusable. The probe is " +
			"being captured rather than called, so an admin turning a subsystem on would " +
			"not reach the client until a restart.")
	}

	enabled = false
	if FeatureStatus()["switchable"] {
		t.Error("turning the subsystem back off did not reach the status map")
	}
}

// TestAPanickingProbeReportsUnavailable keeps one sick subsystem from breaking the
// endpoint that describes all of them.
//
// This endpoint is called on page load. If a probe panics — a half-initialised client, a
// nil map inside a subsystem — the alternative is a 500 on the request the frontend uses
// to decide what to render, which breaks the whole page instead of one feature.
// "Unavailable" is also the honest answer about a subsystem whose health check crashes.
func TestAPanickingProbeReportsUnavailable(t *testing.T) {
	withCleanRegistry(t)

	RegisterFeature("healthy", func() bool { return true })
	RegisterFeature("panicky", func() bool { panic("half-initialised") })

	var status map[string]bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FeatureStatus propagated a panic from a probe: %v", r)
			}
		}()
		status = FeatureStatus()
	}()

	if status["panicky"] {
		t.Error("a probe that panicked was reported as usable")
	}
	if !status["healthy"] {
		t.Error("one panicking probe suppressed an unrelated healthy feature; each probe " +
			"must be isolated from the others")
	}
}

// TestRegisterFeatureIgnoresNonsense checks the two inputs that would otherwise put an
// unusable entry in the registry, one of which would panic on evaluation.
func TestRegisterFeatureIgnoresNonsense(t *testing.T) {
	withCleanRegistry(t)

	RegisterFeature("", func() bool { return true })
	RegisterFeature("nil-probe", nil)

	status := FeatureStatus()
	if _, present := status[""]; present {
		t.Error("an empty feature name was registered; it would appear as an empty JSON key")
	}
	if _, present := status["nil-probe"]; present {
		t.Error("a nil probe was registered; evaluating it would panic")
	}
}

// TestRegisteredFeaturesIsSorted matters because Go randomises map iteration and this
// list is used in diagnostics, where an order that changes between calls reads as churn.
func TestRegisteredFeaturesIsSorted(t *testing.T) {
	withCleanRegistry(t)

	for _, name := range []string{"zulu", "alpha", "mike"} {
		RegisterFeature(name, func() bool { return true })
	}

	for attempt := 0; attempt < 20; attempt++ {
		got := RegisteredFeatures()
		want := []string{"alpha", "mike", "zulu"}
		if len(got) != len(want) {
			t.Fatalf("got %d features, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: got %v, want %v", attempt, got, want)
			}
		}
	}
}

// TestFeatureRegistryIsConcurrencySafe runs under -race. Probes are evaluated on request
// goroutines while a reload can register on another, so an unsynchronised map here would
// be a crash under exactly the conditions that are hardest to reproduce.
func TestFeatureRegistryIsConcurrencySafe(t *testing.T) {
	withCleanRegistry(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterFeature("churn", func() bool { return true })
		}()
		go func() {
			defer wg.Done()
			_ = FeatureStatus()
			_ = RegisteredFeatures()
		}()
	}
	wg.Wait()
}
