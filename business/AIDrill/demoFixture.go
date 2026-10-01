package business

// The drill contributes its own fixture to the curated demo workspace.
//
// WHY THIS FILE EXISTS. The drill shipped, deployed, reachable, and the demo
// workspace had no fixture for it, so every visitor who opened Admin and found
// the one button the whole pitch rests on was shown "Set it up" instead of a
// refusal. The code being live is not the same as the demo being demonstrable,
// and that gap is invisible from the code: nothing fails, the button is simply
// not the thing anyone came to see.
//
// AND THE FIXTURE ALONE IS STILL NOT ENOUGH. A visitor to the public demo is not
// an admin, so the drill button is not theirs to press; what they have is the AI
// filter in Activity, which was empty. The product's whole claim is that a
// refusal is on the record, and the demo's record had nothing in it. So the
// fixture does not only build the channels, it RUNS the drill as the account the
// visitor is logged into, and the refusal is waiting for them when they arrive.
// A real refusal, through the real executor, written to the real chain: nothing
// here is staged for the demo except the timing.
//
// Registered rather than called. business/DemoSeed is on every edition and this
// package is on the AI edition only; a direct call would drag agents onto the
// AI-free line. On that line nothing registers and the seeder runs the rest.

import (
	"context"
	"fmt"
	"os"
	"strings"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// DemoUserEnv names the account the public demo logs its visitors into. The
// controller that mints those sessions reads the same variable, so the account
// the fixture acts as and the account a visitor arrives as cannot drift apart.
const DemoUserEnv = "DEMO_USER_EMAIL"

func init() {
	helpers.RegisterDemoFixture(helpers.DemoFixture{
		Name:     "governance-drill",
		Describe: "two channels the drill refuses across, and a refusal already in the visitor's log",
		Seed:     seedForDemo,
	})
}

// seedForDemo creates the drill fixture as the admin, then leaves a refusal in
// the demo account's own audit trail. Idempotent: Seed returns immediately when
// the channels already exist, and a second run simply adds a fresher refusal.
func seedForDemo(ctx context.Context, adminEmail string) error {
	if adminEmail == "" {
		return fmt.Errorf("no admin to create the drill fixture as")
	}
	admin, err := userDomain.PrincipalByEmail(ctx, adminEmail)
	if err != nil {
		return err
	}
	if err := Seed(ctx, *admin); err != nil {
		return err
	}
	return runForDemoVisitor(ctx)
}

// runForDemoVisitor runs the drill as the account the demo logs visitors into.
//
// It fails the seed when the drill fails, and that is deliberate. This runs on
// every nightly reset, so it is also the only thing that checks the guarantee
// still holds on the deployment strangers are looking at: if a post into a
// channel nobody is in ever succeeds, the reset should stop and say so rather
// than quietly publish a demo of a broken promise.
func runForDemoVisitor(ctx context.Context) error {
	email := strings.TrimSpace(os.Getenv(DemoUserEnv))
	if email == "" {
		// Not a demo host, or the demo account is not configured. The channels
		// are seeded either way, so an admin can still press the button.
		return nil
	}

	pg, err := userDomain.GetUserByEmailId(ctx, &email)
	if err != nil {
		return fmt.Errorf("could not look up the demo account %s: %w", email, err)
	}
	if pg == nil {
		// Nobody has taken the demo yet, so the account the login would create
		// does not exist. Seeding is not the place to invent a user.
		fmt.Printf("fixture note:     %s has never logged in, so there is no log to leave a refusal in\n", email)
		return nil
	}

	visitor, err := userDomain.PrincipalByEmail(ctx, email)
	if err != nil {
		return err
	}

	res, err := Run(ctx, *visitor)
	if err != nil {
		return fmt.Errorf("the governance drill could not run as %s: %w", email, err)
	}
	if !res.Passed {
		return fmt.Errorf("the governance drill did not pass as %s: %s", email, firstFailure(res))
	}
	fmt.Printf("fixture note:     refusal recorded in %s's log, chain checked over %d entries\n",
		email, res.ChainChecked)
	return nil
}

// firstFailure names the step that broke, so the seeder's message says what went
// wrong rather than that something did.
func firstFailure(res *DrillResult) string {
	if res == nil {
		return "the drill returned nothing"
	}
	for _, s := range res.Steps {
		if !s.OK {
			return helpers.FirstNonEmpty(s.Detail, s.Name)
		}
	}
	return "no step reported a failure, which should not be possible when the drill did not pass"
}
