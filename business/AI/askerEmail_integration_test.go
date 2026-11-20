//go:build integration

package business

// V-1191: an agent's audit row names its asker by address; an asker who is
// gone (no row) still renders, with no address, as it always did.
//
// Run: PKGS=./business/AI/ integ_rel.sh <wt> <log> 'TestAnAskerWhoIsGoneRendersWithNoAddress'

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnAskerWhoIsGoneRendersWithNoAddress(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	ada := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'ada@acme.test', NOW(), NOW())`, ada); err != nil {
		t.Fatal(err)
	}
	if got := askerEmail(ctx, uuid.New()); got != "" {
		t.Errorf("an asker who is gone: %q, want no address", got)
	}
	if got := askerEmail(ctx, ada); got != "ada@acme.test" {
		t.Errorf("ada asking: %q, want ada@acme.test", got)
	}
}
