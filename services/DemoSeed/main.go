package demoseed

// The `demoseed` subcommand: curate this workspace into the demo state.
//
// GATED BEHIND AN ENVIRONMENT VARIABLE, and that gate is the whole reason this
// file exists separately from the seeding logic. Every customer runs the same
// binary, so a subcommand that writes invented channels and conversations into a
// workspace is one mistyped command away from putting fake content into somebody's
// real company. ONECAMP_DEMO_HOST is set on the demo host and nowhere else, so the
// command is unreachable on an install that is not the demo, and says why rather
// than doing nothing.
//
// It runs INSIDE the server process rather than over HTTP, unlike the journey
// check: it writes through the business layer, which needs the database, the
// graph and the object store already connected. So it is dispatched after the
// initialisers rather than before them.

import (
	"context"
	"fmt"
	"os"

	demoBusiness "github.com/akashc777/OneCamp/business/DemoSeed"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// EnvGate is the variable that must be set for this to run at all.
const EnvGate = "ONECAMP_DEMO_HOST"

// Allowed reports whether this installation is the demo host.
func Allowed() bool { return os.Getenv(EnvGate) != "" }

// Main curates the workspace and returns a process exit code.
func Main(ctx context.Context, adminEmail string, refresh bool) int {
	if !Allowed() {
		fmt.Fprintf(os.Stderr,
			"demoseed refuses to run: %s is not set.\n\n"+
				"This command writes example channels and conversations into the workspace.\n"+
				"It exists for the public demo at onecamp.onemana.dev, which is reset nightly,\n"+
				"and it is not something to run against a workspace anybody relies on.\n", EnvGate)
		return 2
	}
	if adminEmail == "" {
		fmt.Fprintln(os.Stderr, "demoseed needs the admin to act as: demoseed <admin-email>")
		return 2
	}

	if code := runFixtures(ctx, adminEmail); code != 0 {
		return code
	}

	admin, err := userDomain.PrincipalByEmail(ctx, adminEmail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "demoseed could not load %s: %v\n", adminEmail, err)
		return 1
	}

	res, err := demoBusiness.Run(ctx, *admin, refresh)
	if res != nil {
		fmt.Printf("channels created: %d\nposts written:    %d\nchannels archived: %d\n",
			res.ChannelsCreated, res.PostsWritten, res.ChannelsArchived)
		for _, s := range res.Skipped {
			fmt.Printf("left alone:       %s (already exists)\n", s)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "demoseed failed: %v\n", err)
		return 1
	}
	return 0
}

// runFixtures creates whatever any linked subsystem contributed, before the
// channels.
//
// Its own function so it can be tested without a database. The drill's fixture is
// the thing a visitor came to see, and a seeder that created the conversation but
// not the fixture would leave the demo looking curated and still not
// demonstrable — which is exactly the state the drill shipped in.
func runFixtures(ctx context.Context, adminEmail string) int {
	for _, f := range helpers.DemoFixtures() {
		if err := f.Seed(ctx, adminEmail); err != nil {
			fmt.Fprintf(os.Stderr, "fixture %s failed: %v\n", f.Name, err)
			return 1
		}
		fmt.Printf("fixture:          %s (%s)\n", f.Name, f.Describe)
	}
	return 0
}
