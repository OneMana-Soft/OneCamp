//go:build integration
// +build integration

package integration_test

// When did this install first run each importer fix? A server records each
// once, as it starts (importBusiness.ClaimImportFixes):
//   - Todoist and Notion adding only the people a project is shared with;
//   - Jira keeping issue security levels, and the comments it keeps to some
//     people;
//   - Basecamp adding no client as a member;
//   - Linear bringing non-public teams' issues in restricted.
// An import whose first run started before its fix's moment came in under
// the old rule. Later releases read the records by their keys, so the keys'
// names are pinned here.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEachImportFixIsClaimedOnce -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEachImportFixIsClaimedOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	keys := []string{"imports.membership_fixed", "imports.jira_restrictions_fixed", "imports.basecamp_clients_fixed", "imports.linear_private_teams_fixed"}
	type record struct {
		at    time.Time
		value string
	}
	records := func(when string) map[string]record {
		t.Helper()
		out := map[string]record{}
		for _, key := range keys {
			cfg, err := configModel.GetConfigByKey(key)
			if err != nil || cfg == nil {
				t.Fatalf("%s left no record under %q: %v", when, key, err)
			}
			out[key] = record{cfg.UpdatedAt, cfg.Value}
		}
		return out
	}

	if err := importBusiness.ClaimImportFixes(ctx); err != nil {
		t.Fatalf("the first start's claim: %v", err)
	}
	first := records("the first start")

	// A later start, or a second replica starting beside it, claims too and
	// moves nothing: each first time stands.
	if err := importBusiness.ClaimImportFixes(ctx); err != nil {
		t.Fatalf("a later start's claim: %v", err)
	}
	for key, again := range records("a later start") {
		if was := first[key]; !again.at.Equal(was.at) || again.value != was.value {
			t.Errorf("a later start moved %q to %v %q from %v %q", key, again.at, again.value, was.at, was.value)
		}
	}

	// The store's claim answers whether it wrote: only on a key nobody holds.
	if wrote, err := configModel.Claim(ctx, keys[0], "a later value"); err != nil || wrote {
		t.Fatalf("a claim on a held key wrote: %v (%v)", wrote, err)
	}
	if wrote, err := configModel.Claim(ctx, "tests.claim."+uuid.NewString(), "first"); err != nil || !wrote {
		t.Fatalf("a claim on a free key didn't write: %v (%v)", wrote, err)
	}
}
