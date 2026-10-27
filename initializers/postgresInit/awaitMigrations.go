package postgresInit

// A fresh install brings its server up before it creates the database and
// migrates it (`make install`: build_restart_all, then create_postgres_db,
// then migrate_up, which builds sqlx-cli first and takes minutes). The server
// restarts until it can connect, and then used to start on the empty
// database: the schema check found no ledger, warned and went on, and the
// start-up claims after it (the automation bot, the moment of each importer
// fix, the command catalog, the MCP registry) failed once against the missing
// tables and were never tried again, since the server ran on. So the check
// first waits for the migrations on a database none has reached yet, and
// everything after it starts on the schema it was built for.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// migrationPoll is how the wait sleeps between looks at the ledger:
// time.After, a seam so a test can migrate at the moment the wait looks.
var migrationPoll = time.After

const (
	// migrationPollEvery is how often the wait looks at the ledger.
	migrationPollEvery = 5 * time.Second
	// migrationWaitSaysEvery is how many looks pass between the lines that
	// say the wait goes on.
	migrationWaitSaysEvery = 12
)

// awaitMigrations waits, on a fresh database, until its ledger records every
// migration in required as applied, or one as failed (the verdict after it
// names which). It returns at once on any other database, so one whose ledger
// can't be read, or that another tool migrated, starts as before. It stops
// waiting when ctx ends.
func awaitMigrations(ctx context.Context, db *sql.DB, required []int64) error {
	if db == nil || !freshDatabase(ctx, db, required) {
		return nil
	}
	helpers.MessageLogs.InfoLog.Printf("No migration has reached this database yet: waiting for them before starting (run %s)", MigrateCommand())
	began := time.Now()
	for looks := 1; ; looks++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-migrationPoll(migrationPollEvery):
		}
		state := InspectSchema(ctx, db, required)
		if state.Determined && (!state.Behind() || len(state.FailedVersions) > 0) {
			helpers.MessageLogs.InfoLog.Printf("The database's migrations are in after %s: starting", time.Since(began).Round(time.Second))
			return nil
		}
		if looks%migrationWaitSaysEvery == 0 {
			applied := 0
			if state.Determined {
				applied = len(required) - len(state.Missing())
			}
			helpers.MessageLogs.InfoLog.Printf("Still waiting for the database's migrations after %s: %d of %d applied (run %s)",
				time.Since(began).Round(time.Second), applied, len(required), MigrateCommand())
		}
	}
}

// freshDatabase is whether no migration has reached db: its ledger records
// none as applied, and it holds no table besides the ledger.
func freshDatabase(ctx context.Context, db *sql.DB, required []int64) bool {
	if InspectSchema(ctx, db, required).Determined {
		return false
	}
	// pg_tables lists every table, whoever may read it.
	var tables int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname = current_schema() AND tablename <> $1`, sqlxMigrationsTable).Scan(&tables)
	return err == nil && tables == 0
}
