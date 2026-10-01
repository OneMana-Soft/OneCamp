package demoseed

import (
	"context"
	"os"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// The gate is the safety property, so it is tested first and hardest.
//
// Every customer runs this binary. A subcommand that writes invented channels and
// conversations into a workspace is one mistyped word away from putting fake
// content into somebody's real company, and the only thing standing between those
// two outcomes is this check.
func TestRefusesWhenNotTheDemoHost(t *testing.T) {
	t.Setenv(EnvGate, "")
	if Allowed() {
		t.Fatal("an install with no demo marker reported itself as the demo host")
	}
	// Even given a perfectly good admin address, it must not proceed.
	if code := Main(context.Background(), "admin@example.test", false); code == 0 {
		t.Error("demoseed returned success on an install that is not the demo host; " +
			"on a customer workspace that would have written example content into it")
	}
}

func TestAllowedOnlyWithTheMarkerSet(t *testing.T) {
	t.Setenv(EnvGate, "")
	if Allowed() {
		t.Error("empty marker must not allow")
	}
	t.Setenv(EnvGate, "1")
	if !Allowed() {
		t.Error("marker set must allow")
	}
}

// A missing admin must be refused before anything is written, not discovered
// half way through a workspace.
func TestRefusesWithoutAnAdminToActAs(t *testing.T) {
	t.Setenv(EnvGate, "1")
	if code := Main(context.Background(), "", false); code == 0 {
		t.Error("demoseed proceeded with no admin address")
	}
}

// The marker name must stay something a customer would never set by accident.
func TestGateNameIsSpecificToTheDemo(t *testing.T) {
	if EnvGate == "" || len(EnvGate) < 12 {
		t.Fatalf("gate variable %q is too generic to be safe", EnvGate)
	}
	for _, generic := range []string{"DEBUG", "DEV", "TEST", "SEED", "ENV"} {
		if EnvGate == generic {
			t.Errorf("gate variable %q is a name a customer might already have set", EnvGate)
		}
	}
	if os.Getenv(EnvGate) != "" {
		t.Logf("note: %s is set in this environment", EnvGate)
	}
}

// Registered fixtures must actually run.
//
// The fixture registry exists because the demo shipped a drill nobody could see:
// live, reachable, and with no fixture in the workspace, so the one button the
// pitch rests on said "Set it up" to every visitor. A registry the seeder forgets
// to read would reproduce that exactly, and silently.
//
// Reachable without a database because the loop runs before the admin lookup:
// each fixture resolves its own principal, so nothing is gained by ordering it
// after, and a great deal is gained by being able to test it.
func TestRegisteredFixturesRunBeforeAnythingElse(t *testing.T) {
	t.Setenv(EnvGate, "1")

	ran := false
	gotEmail := ""
	helpers.RegisterDemoFixture(helpers.DemoFixture{
		Name:     "test-fixture",
		Describe: "records that it ran",
		Seed: func(_ context.Context, adminEmail string) error {
			ran = true
			gotEmail = adminEmail
			return nil
		},
	})

	if code := runFixtures(context.Background(), "admin@example.test"); code != 0 {
		t.Fatalf("runFixtures returned %d for a fixture that succeeds", code)
	}

	if !ran {
		t.Fatal("a registered fixture did not run, so a curated demo would be created " +
			"without the thing it exists to show")
	}
	if gotEmail != "admin@example.test" {
		t.Errorf("fixture received admin %q, want the address passed to Main", gotEmail)
	}
}

// A fixture that fails must stop the run rather than leaving a half-curated
// workspace that looks finished.
func TestAFailingFixtureStopsTheRun(t *testing.T) {
	t.Setenv(EnvGate, "1")
	helpers.RegisterDemoFixture(helpers.DemoFixture{
		Name:     "failing-fixture",
		Describe: "always fails",
		Seed:     func(context.Context, string) error { return errFixture },
	})
	if code := runFixtures(context.Background(), "admin@example.test"); code == 0 {
		t.Error("a failing fixture did not stop the seeder")
	}
}

var errFixture = errTest("fixture blew up")

type errTest string

func (e errTest) Error() string { return string(e) }
