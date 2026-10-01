package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// Importing this package must register the drill's demo fixture.
//
// THE GAP THIS CLOSES. The drill shipped and deployed and the demo workspace had
// no fixture for it, so the one button the whole pitch rests on said "Set it up"
// to every visitor. Nothing failed; the demo was simply not demonstrable, which
// is invisible from the code and obvious to the buyer.
//
// The registration happens in init(), so this test passing is the same event that
// makes the fixture exist in the server binary.
func TestImportingTheDrillRegistersItsDemoFixture(t *testing.T) {
	for _, f := range helpers.DemoFixtures() {
		if f.Name == "governance-drill" {
			if f.Seed == nil {
				t.Fatal("the drill fixture is registered with no seed function")
			}
			if f.Describe == "" {
				t.Error("the fixture has no description, so the seeder's output does not " +
					"say what it created")
			}
			return
		}
	}
	t.Fatal("importing business/AIDrill did not register a governance-drill demo fixture, " +
		"so a curated demo workspace would be created without the thing it exists to show")
}

// A demo host is the only install where the drill should act as somebody other
// than the caller, and DEMO_USER_EMAIL is what says so. Unset, the fixture must
// not go looking for an account to impersonate: every customer runs this binary.
func TestNoDemoAccountConfiguredMeansNobodyIsActedAs(t *testing.T) {
	t.Setenv(DemoUserEnv, "")
	if err := runForDemoVisitor(context.Background()); err != nil {
		t.Fatalf("with no demo account configured the fixture must do nothing, got: %v", err)
	}
}

// The seeder's message has to name the step that broke. "The drill did not pass"
// sends whoever reads the nightly reset log back to the database to find out
// what, which is the whole cost this saves.
func TestTheFailureMessageNamesTheStep(t *testing.T) {
	res := &DrillResult{Steps: []StepResult{
		{Name: "the person is not a member", OK: true},
		{Name: "the post is refused", OK: false, Detail: "THE POST SUCCEEDED"},
	}}
	if got := firstFailure(res); got != "THE POST SUCCEEDED" {
		t.Errorf("wanted the failing step's detail, got %q", got)
	}

	// A step with no detail still has to name itself rather than return nothing.
	res.Steps[1].Detail = ""
	if got := firstFailure(res); got != "the post is refused" {
		t.Errorf("wanted the failing step's name, got %q", got)
	}
}

func TestAPassingDrillHasNoFailureToReport(t *testing.T) {
	res := &DrillResult{Steps: []StepResult{{Name: "all good", OK: true}}}
	if got := firstFailure(res); got == "" {
		t.Error("firstFailure must always say something, even when asked about a pass")
	}
}
