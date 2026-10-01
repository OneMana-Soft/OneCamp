package postgresInit

// schemaVersion.go — refuse to run against a database the binary has outgrown.
//
// WHY THIS EXISTS. A deploy shipped a binary needing migrations 133–137 against a
// database still at 132. Nothing noticed. The server logged "Server started on
// port 3000" and then, every five seconds, forever:
//
//	agentTaskWorker: reclaim leases failed: column "cancel_requested_at" does not exist
//	agentTaskWorker: claim failed: column t.cancel_requested_at does not exist
//
// Every durable agent run was dead, the AI configuration silently fell back to
// environment variables (discarding whatever an admin had set in the UI), and
// user-facing requests returned 500s — while the process reported itself healthy
// and produced roughly fifty thousand identical log lines a day, which buries any
// real error and eventually fills the disk.
//
// A schema mismatch is not a degraded mode worth limping through: the code and the
// data disagree about what exists, so the blast radius is unknowable. Failing at
// boot with the exact gap and the exact command turns a silent, permanent
// half-outage into a deploy that visibly fails and takes a minute to fix.
//
// SHAPE OF THE CHECK, chosen so it cannot itself cause an outage:
//   - it only refuses to start when it can POSITIVELY establish the database is
//     behind. Anything indeterminate — no migrations table, an unreadable table, a
//     query error — warns and continues, because a broken check must never take
//     down a healthy server.
//   - a database AHEAD of the binary warns but continues, so rolling a release
//     back is still possible.
//   - ALLOW_SCHEMA_DRIFT=true overrides the refusal, because an operator in an
//     incident needs a way through that does not involve rebuilding.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	// The module root, which embeds the migration set. Aliased because the package is named
	// onecamp while the final path element is OneCamp.
	onecamp "github.com/akashc777/OneCamp"
)

// RequiredSchemaVersion is the highest migration this binary needs applied.
//
// DERIVED FROM THE EMBEDDED MIGRATIONS, not hand-maintained. It was a constant that had to be bumped
// by hand with every migration, and the only thing between forgetting and a silently weakened guard was
// a test remembering to fail. The number now comes from the files themselves.
//
// The comment here used to explain the constant by saying go:embed "cannot reach" the migrations
// directory. That was half true and led to the wrong conclusion. Patterns may not contain "..", so this
// package cannot reach UPWARD — but they reach downward freely, and the module root sits above
// migrations/, which is where the embed now lives (see package onecamp in schema.go).
//
// The real constraint was never go:embed. It is that the RUNNING SERVER HAS NO MIGRATIONS DIRECTORY:
// the container ships a binary, and migrations/ is bind-mounted only into the throwaway container that
// runs sqlx. So the ceiling has to be fixed at build time by some means — a constant was one, an embed
// is the better one, because it cannot disagree with the files it was built from.
var RequiredSchemaVersion = onecamp.HighestMigration()

// sqlxMigrationsTable is the ledger sqlx-cli maintains (see `make migrate_up`).
const sqlxMigrationsTable = "_sqlx_migrations"

// SchemaState is what we could determine about the database's migration level.
type SchemaState struct {
	// Determined is false when the check could not establish a version at all. The
	// caller must treat that as "unknown", never as "behind".
	Determined bool
	// Applied is the highest SUCCESSFULLY applied migration version.
	Applied int64
	// Required is the version this binary needs.
	Required int64
	// FailedVersions are migrations recorded as attempted but not successful, which
	// is a distinct and worse situation than simply being behind: the database may
	// be half-migrated.
	FailedVersions []int64
}

// Behind reports whether the database is positively known to be older than the
// binary requires.
func (s SchemaState) Behind() bool { return s.Determined && s.Applied < s.Required }

// Ahead reports whether the database has migrations this binary does not know
// about, which happens on a rollback.
func (s SchemaState) Ahead() bool { return s.Determined && s.Applied > s.Required }

// Missing lists the migration versions between what is applied and what is
// required, for a message an operator can act on without going digging.
func (s SchemaState) Missing() []int64 {
	if !s.Behind() {
		return nil
	}
	out := make([]int64, 0, s.Required-s.Applied)
	for v := s.Applied + 1; v <= s.Required; v++ {
		out = append(out, v)
	}
	return out
}

// InspectSchema reads the migration ledger. It returns a state rather than an
// error for the "cannot tell" cases, so the caller's policy stays in one place.
func InspectSchema(ctx context.Context, db *sql.DB, required int64) SchemaState {
	state := SchemaState{Required: required}
	if db == nil {
		return state
	}

	// Does the ledger exist? A fresh database, or one migrated by another tool, is
	// indeterminate rather than behind.
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		sqlxMigrationsTable,
	).Scan(&exists); err != nil || !exists {
		return state
	}

	var applied sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM `+sqlxMigrationsTable+` WHERE success = true`,
	).Scan(&applied); err != nil {
		return state
	}
	if !applied.Valid {
		// The ledger exists but records nothing successful. Indeterminate: this is
		// what a database mid-bootstrap looks like.
		return state
	}
	state.Determined = true
	state.Applied = applied.Int64

	// A migration recorded as unsuccessful means a previous run died partway. Worth
	// naming separately, because "run the migrations" is not the whole fix.
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM `+sqlxMigrationsTable+` WHERE success = false ORDER BY version`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var v int64
			if scanErr := rows.Scan(&v); scanErr == nil {
				state.FailedVersions = append(state.FailedVersions, v)
			}
		}
		_ = rows.Err()
	}
	return state
}

// SchemaDriftMessage renders the operator-facing explanation for a state. Pure, so
// the wording is testable and stays actionable.
func SchemaDriftMessage(s SchemaState) string {
	var b strings.Builder
	if len(s.FailedVersions) > 0 {
		fmt.Fprintf(&b, "database migrations %s are recorded as FAILED (a previous migration run did not finish); ",
			joinVersions(s.FailedVersions))
	}
	switch {
	case s.Behind():
		fmt.Fprintf(&b, "database schema is at migration %d but this build requires %d. Missing: %s. ",
			s.Applied, s.Required, joinVersions(s.Missing()))
		fmt.Fprintf(&b, "Apply them with `%s`, then restart this service. ", MigrateCommand())
		b.WriteString("Refusing to start: running against an older schema breaks durable agent runs, " +
			"silently discards admin AI settings in favour of env defaults, and returns 500s to users, " +
			"while the process looks healthy. Set ALLOW_SCHEMA_DRIFT=true to start anyway.")
	case s.Ahead():
		fmt.Fprintf(&b, "database schema is at migration %d, AHEAD of this build's %d. ",
			s.Applied, s.Required)
		b.WriteString("Continuing, since this is what a rollback looks like, but columns this build does not " +
			"know about exist and a newer build may be expected.")
	default:
		b.WriteString("database schema version could not be determined; continuing without the check.")
	}
	return b.String()
}

func joinVersions(vs []int64) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, strconv.FormatInt(v, 10))
	}
	return strings.Join(parts, ", ")
}

// MigrateCommand returns the make target that applies migrations FOR THE STACK THIS PROCESS IS
// RUNNING IN.
//
// WHY THIS IS NOT A CONSTANT. The message used to hardcode `make migrate_server_up`, which is the
// shared/beta target. On a host that also runs per-customer stacks that instruction is wrong in a
// way that costs real time and can cost real data:
//
//   - It is not the customer target. The customer stack's postgres is on its own `internal`
//     network with no published port, so the shared target cannot reach it.
//   - Worse, it may reach something else. migrate_server_up resolves its network by grepping
//     `docker network ls` for onecamp-shared-net and taking the first match, and a customer stack
//     joins that network too. Run from a customer directory it can attach to the shared network,
//     where `postgres` then resolves to the OTHER stack's database — migrating a tenant nobody
//     asked about.
//
// CID is the discriminator because it already exists: it is written into a customer .env,
// The compose file loads that file into the service, and the
// shared/beta env has no CID at all. So this reads a fact about the deployment rather than adding
// a flag someone has to remember to set.
func MigrateCommand() string {
	if cid := strings.TrimSpace(os.Getenv("CID")); cid != "" {
		return "make customer_migrate_up CID=" + cid
	}
	return "make migrate_server_up"
}

// AllowSchemaDrift reports whether an operator has explicitly opted out of the
// refusal. Any parseable-true value counts.
func AllowSchemaDrift() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("ALLOW_SCHEMA_DRIFT")))
	return err == nil && v
}

// VerifySchemaAtStartup returns an error ONLY when the database is positively
// behind and the operator has not opted out. Every other outcome returns a
// (possibly empty) advisory message for the caller to log.
//
// Mirrors the existing startup validators (import/AI key encryption): the caller
// logs and exits, so boot policy stays visible in main.
func VerifySchemaAtStartup(ctx context.Context) (advisory string, err error) {
	state := InspectSchema(ctx, DBConn.SqlDB, RequiredSchemaVersion)
	switch {
	case state.Behind():
		msg := SchemaDriftMessage(state)
		if AllowSchemaDrift() {
			return "ALLOW_SCHEMA_DRIFT is set, starting anyway. " + msg, nil
		}
		return "", fmt.Errorf("%s", msg)
	case state.Ahead(), len(state.FailedVersions) > 0:
		return SchemaDriftMessage(state), nil
	default:
		return "", nil
	}
}
