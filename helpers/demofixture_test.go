package helpers

import (
	"context"
	"testing"
)

func TestDemoFixtureRegistryIsOrderedAndSafe(t *testing.T) {
	before := len(DemoFixtures())

	RegisterDemoFixture(DemoFixture{Name: "zebra", Seed: func(context.Context, string) error { return nil }})
	RegisterDemoFixture(DemoFixture{Name: "alpha", Seed: func(context.Context, string) error { return nil }})
	// Nameless and seedless registrations are ignored rather than stored as a
	// fixture that cannot run.
	RegisterDemoFixture(DemoFixture{Name: "", Seed: func(context.Context, string) error { return nil }})
	RegisterDemoFixture(DemoFixture{Name: "no-seed"})

	got := DemoFixtures()
	if len(got) != before+2 {
		t.Fatalf("registry holds %d fixtures, want %d; an entry with no name or no seed "+
			"must not be stored", len(got), before+2)
	}
	// Stable order, so two runs seed in the same sequence.
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("fixtures are not in a stable order: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

func TestRegisteringTheSameFixtureTwiceSeedsOnce(t *testing.T) {
	calls := 0
	seed := func(context.Context, string) error { calls++; return nil }
	RegisterDemoFixture(DemoFixture{Name: "dup-check", Seed: seed})
	RegisterDemoFixture(DemoFixture{Name: "dup-check", Seed: seed})

	found := 0
	for _, f := range DemoFixtures() {
		if f.Name == "dup-check" {
			found++
			_ = f.Seed(context.Background(), "a@b.test")
		}
	}
	if found != 1 {
		t.Errorf("a fixture registered twice appears %d times; it would seed twice", found)
	}
	if calls != 1 {
		t.Errorf("seed ran %d times for one fixture", calls)
	}
}
