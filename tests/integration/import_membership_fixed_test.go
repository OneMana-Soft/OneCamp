//go:build integration
// +build integration

package integration_test

// When did this install's Todoist and Notion imports begin to add only the
// people each project is shared with? A server records it once, as it starts
// (importBusiness.ClaimMembershipFixed); an import whose first run started
// before then is one an admin may need to review. A later release reads the
// record by its key, so the key's name is pinned here.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheImportMembershipFixIsClaimedOnce -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheImportMembershipFixIsClaimedOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	const key = "imports.membership_fixed"

	if err := importBusiness.ClaimMembershipFixed(ctx); err != nil {
		t.Fatalf("the first start's claim: %v", err)
	}
	first, err := configModel.GetConfigByKey(key)
	if err != nil || first == nil {
		t.Fatalf("the first start left no record under %q: %v", key, err)
	}

	// A later start, or a second replica starting beside it, claims too and
	// moves nothing: the first time stands.
	if err := importBusiness.ClaimMembershipFixed(ctx); err != nil {
		t.Fatalf("a later start's claim: %v", err)
	}
	again, err := configModel.GetConfigByKey(key)
	if err != nil || again == nil {
		t.Fatalf("the record went: %v", err)
	}
	if !again.UpdatedAt.Equal(first.UpdatedAt) || again.Value != first.Value {
		t.Fatalf("a later start moved the record to %v %q from %v %q", again.UpdatedAt, again.Value, first.UpdatedAt, first.Value)
	}

	// The store's claim answers whether it wrote: only on a key nobody holds.
	if wrote, err := configModel.Claim(ctx, key, "a later value"); err != nil || wrote {
		t.Fatalf("a claim on a held key wrote: %v (%v)", wrote, err)
	}
	if wrote, err := configModel.Claim(ctx, "tests.claim."+uuid.NewString(), "first"); err != nil || !wrote {
		t.Fatalf("a claim on a free key didn't write: %v (%v)", wrote, err)
	}
}
