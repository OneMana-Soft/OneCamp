//go:build integration

package business

// V-1191: the person steering an agent run is named on the run's notes; one
// who is gone (no row) still renders, with no name, as it always did. (The
// read it makes selects no name columns, so today every author renders
// nameless and the caller falls back: a separate finding, not this head's.)
//
// Run: PKGS=./business/AIAgent/ integ_rel.sh <wt> <log> 'TestASteeringAuthorWhoIsGoneRendersNameless'

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestASteeringAuthorWhoIsGoneRendersNameless(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	if got := steeringAuthorName(ctx, uuid.New()); got != "" {
		t.Errorf("a steering author who is gone: %q, want no name", got)
	}
}
