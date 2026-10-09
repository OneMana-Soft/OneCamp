//go:build integration
// +build integration

package integration_test

// Migration 203 on a workspace that already has data.
//
// Accounts and invitations are now found by address without regard to case,
// through indexes on LOWER(email_id) and LOWER(email). An install that
// predates lowercasing can hold two accounts whose addresses differ only in
// case. A unique index would fail on those and stop the upgrade, so the
// indexes are plain ones; this runs the real migration tool on Postgres 12,
// down to 202, with such rows in place, and back up. Both accounts keep
// working, and the admin's system check names the address.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEmailLookupIndex -v

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEmailLookupIndexSurvivesAddressesThatDifferOnlyInCase(t *testing.T) {
	env := integration.SetupEnv(t)
	var version string
	if err := env.PG.QueryRow(`SHOW server_version`).Scan(&version); err != nil || !strings.HasPrefix(version, "12") {
		t.Fatalf("server version %q (%v): this is meant to run on Postgres 12, as production does", version, err)
	}

	_, here, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(here), "..", "..", "migrations"), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	indexes := func() (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes
			WHERE indexname IN ('users_email_id_lower_idx', 'invitations_email_lower_idx')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Where an existing workspace is before upgrading.
	if err := m.Migrate(202); err != nil {
		t.Fatalf("down to 202: %v", err)
	}
	if indexes() != 0 {
		t.Fatal("the down migration left an index behind")
	}
	for _, email := range []string{"Dup@Example.test", "dup@example.test"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, created_at, updated_at) VALUES ($1, $2, $3, NOW(), NOW())`,
			uuid.New(), email, "u-"+uuid.NewString()[:8]); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
		if _, err := env.PG.Exec(`INSERT INTO invitations (email, status, token) VALUES ($1, 'sent', $2)`, email, uuid.NewString()); err != nil {
			t.Fatalf("seed an invitation for %s: %v", email, err)
		}
	}

	if err := m.Migrate(203); err != nil {
		t.Fatalf("migration 203 with addresses that differ only in case: %v", err)
	}
	if n := indexes(); n != 2 {
		t.Fatalf("after 203 there are %d of the 2 indexes", n)
	}
	// Both accounts work: a lookup reaches the one spelt exactly as asked, and
	// the admin's system check names the address so a person can decide.
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	for _, asked := range []string{"Dup@Example.test", "dup@example.test"} {
		u, err := userDomain.GetUserByEmailId(ctx, &asked)
		if err != nil || u == nil || u.EmailID != asked {
			t.Errorf("looking up %q reached %+v (%v), want the account spelt that way", asked, u, err)
		}
	}
	shared, err := userDomain.AddressesWithMoreThanOneAccount(ctx, 5)
	if err != nil || len(shared) != 1 || shared[0] != "dup@example.test" {
		t.Errorf("addresses with more than one account: %v (%v), want [dup@example.test]", shared, err)
	}

	// And back down and up again, as a rollback would.
	if err := m.Migrate(202); err != nil || indexes() != 0 {
		t.Fatalf("rolling 203 back: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("up again: %v", err)
	}
	if indexes() != 2 {
		t.Fatal("up again did not bring the indexes back")
	}
}
