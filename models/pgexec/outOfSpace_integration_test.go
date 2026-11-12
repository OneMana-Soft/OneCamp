//go:build integration

package pgexec_test

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/akashc777/OneCamp/tests/integration"
)

// spaceSecret is a value the refused statements carry, which no log line may hold.
const spaceSecret = "v583-value-never-logged"

// Every statement Postgres refuses for space (SQLSTATE 53100) is named in the
// log by the code that ran it, never by its text or its values (V-583, main's
// ruling 2): one write of 20,442 on the 64M load test was refused, and its log
// line couldn't say which (V-610). The name is the first caller outside the
// database layers, so a raw transaction, gorm and pgexec are all named, and a
// refused statement is said once, however often its kind runs out in a minute:
// pgexec's run again of a read, refused again, adds no second line.
func TestAStatementOutOfSpaceIsNamedInTheLogByItsCaller(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	logged := catchWarnings(t)
	for _, s := range []string{
		`CREATE TABLE space_t (v text NOT NULL)`,
		`CREATE FUNCTION space_gone(v text) RETURNS text LANGUAGE plpgsql VOLATILE AS $$ BEGIN
			RAISE EXCEPTION 'could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device' USING ERRCODE = '53100';
		END $$`,
	} {
		if _, err := pgexec.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	refusals := []struct {
		kind string
		run  func(ctx context.Context) error
	}{
		{"models/pgexec_test.refusedInARawTransaction", refusedInARawTransaction},
		{"models/pgexec_test.refusedThroughGorm", refusedThroughGorm},
		{"models/pgexec_test.refusedThroughPgexec", refusedThroughPgexec},
	}
	for _, r := range refusals {
		for i := 0; i < 3; i++ {
			if err := r.run(ctx); !helpers.IsDiskFull(err) {
				t.Fatalf("%s: %v; want its 53100", r.kind, err)
			}
		}
	}

	lines := logged.lines()
	for _, r := range refusals {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "out of space (53100)") && strings.Contains(l, " in "+r.kind+":") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d log lines naming it out of space; want 1 for its 3 refusals\n%s", r.kind, n, strings.Join(lines, "\n"))
		}
	}
	for _, l := range lines {
		for _, never := range []string{spaceSecret, "space_gone", "space_t", "SELECT", "INSERT"} {
			if strings.Contains(l, never) {
				t.Errorf("a log line holds %q, a refused statement's text or value; want only its caller's name:\n%s", never, l)
			}
		}
	}
	if n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM space_t`); err != nil || n != 0 {
		t.Errorf("%d rows written by refused writes, %v; want none", n, err)
	}
}

// refusedInARawTransaction writes through database/sql's own transaction, as
// the row ledger does.
func refusedInARawTransaction(ctx context.Context) error {
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO space_t SELECT $1 WHERE space_gone($1) IS NOT NULL`, spaceSecret)
	return err
}

// refusedThroughGorm reads through gorm.
func refusedThroughGorm(ctx context.Context) error {
	var s string
	return postgresInit.DBConn.GormDB.WithContext(ctx).Raw(`SELECT space_gone(?)`, spaceSecret).Scan(&s).Error
}

// refusedThroughPgexec reads through pgexec outside a transaction, which runs
// it once more without parallel workers, and is refused again.
func refusedThroughPgexec(ctx context.Context) error {
	_, err := pgexec.Scalar[string](ctx, `SELECT space_gone($1)`, spaceSecret)
	return err
}
