//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A fresh install brings its server up before it creates the database and
// migrates it (`make install`), so the server's first start meets a database
// no migration has reached. Its schema check waits for the migrations, so the
// claims main makes after it still land: the automation bot, in Postgres and
// in the graph, and the moment of each importer fix.
func TestAFirstStartBeforeTheMigrationsStillSeedsTheBotAndTheImportMarkers(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupUnmigratedEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	dg := integration.SetupDgraph(t)
	migrated := false
	migrate := func() {
		if !migrated {
			migrated = true
			env.MigrateAsTheInstallDoes(t)
		}
	}
	// The install migrates while the start waits, before its next look.
	t.Cleanup(postgresInit.SetMigrationPollForTest(func(time.Duration) <-chan time.Time {
		migrate()
		now := make(chan time.Time, 1)
		now <- time.Now()
		return now
	}))

	// main's order: the schema check, then the claims.
	if advisory, err := postgresInit.VerifySchemaAtStartup(ctx); err != nil {
		t.Fatalf("the schema check: %v (%s)", err, advisory)
	}
	_, botErr := userBusiness.EnsureAutomationBot(ctx)
	fixesErr := importBusiness.ClaimImportFixes(ctx)
	// A start that didn't wait has made its claims on no schema by now.
	migrate()

	var bots int
	err := env.PG.QueryRow(`SELECT count(*) FROM users WHERE email_id = $1 AND deleted_at IS NULL`, userDomain.SystemBotEmail).Scan(&bots)
	if err != nil || bots != 1 || botErr != nil {
		t.Errorf("the automation bot in Postgres: %d rows (%v; seeding it: %v); want it seeded", bots, err, botErr)
	}
	if got := string(dg.Query(t, `{ bot(func: eq(user_email_id, "`+userDomain.SystemBotEmail+`")) { uid } }`)); !strings.Contains(got, `"uid"`) {
		t.Errorf("the automation bot in the graph: %s; want its node", got)
	}
	for _, key := range []string{importBusiness.MembershipFixed, importBusiness.JiraRestrictionsFixed,
		importBusiness.BasecampClientsFixed, importBusiness.LinearPrivateTeamsFixed} {
		if _, ok, err := importBusiness.FixedAt(key); err != nil || !ok || fixesErr != nil {
			t.Errorf("%s claimed: %v (%v; claiming: %v); want the first start to claim it", key, ok, err, fixesErr)
		}
	}
}

// A database that holds the schema but no sqlx ledger (one another tool
// migrated, as golang-migrate migrates every test's) is not waited for: the
// check says it can't tell, and the start goes on as before.
func TestAStartOnATableFullDatabaseWithNoLedgerDoesNotWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	waited := false
	t.Cleanup(postgresInit.SetMigrationPollForTest(func(time.Duration) <-chan time.Time {
		waited = true
		cancel()
		return make(chan time.Time)
	}))

	advisory, err := postgresInit.VerifySchemaAtStartup(ctx)
	if waited || err != nil || !strings.Contains(advisory, "could not be determined") {
		t.Errorf("the check: waited %v, %v, advisory %q; want no wait and the advisory that it can't tell", waited, err, advisory)
	}
}
