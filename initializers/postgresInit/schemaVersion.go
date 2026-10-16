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
//   - it compares SETS: every migration this build embeds against every one the
//     ledger records as applied. Not the highest of each, because sqlx applies
//     whatever is missing, lower versions included, so a database can hold the
//     highest version and still lack one below it: a hotfix numbered above work
//     that has not shipped yet, or a programme's reserved range (the 300s),
//     lands in the ledger first. Compared by maximum, every gap below it read as
//     applied.
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
	"slices"
	"strconv"
	"strings"

	// The module root, which embeds the migration set. Aliased because the package is named
	// onecamp while the final path element is OneCamp.
	onecamp "github.com/akashc777/OneCamp"
)

// RequiredMigrations is every migration this binary needs applied, ascending: what the check
// compares the ledger against.
//
// DERIVED FROM THE EMBEDDED MIGRATIONS, not hand-maintained. It was a constant that had to be bumped
// by hand with every migration, and the only thing between forgetting and a silently weakened guard was
// a test remembering to fail. The versions now come from the files themselves.
//
// The real constraint was never go:embed. It is that the RUNNING SERVER HAS NO MIGRATIONS DIRECTORY:
// the container ships a binary, and migrations/ is bind-mounted only into the throwaway container that
// runs sqlx. So the set has to be fixed at build time, and an embed (package onecamp, schema.go) cannot
// disagree with the files it was built from. It sits at the module root because go:embed patterns may
// not reach upward with "..".
var RequiredMigrations = onecamp.Migrations()

// sqlxMigrationsTable is the ledger sqlx-cli maintains (see `make migrate_up`).
const sqlxMigrationsTable = "_sqlx_migrations"

// SchemaState is what we could determine about the database's migrations.
type SchemaState struct {
	// Determined is false when the check could not establish what is applied at all. The
	// caller must treat that as "unknown", never as "behind".
	Determined bool
	// Applied is the highest SUCCESSFULLY applied migration version, and Required this build's
	// highest. Both are for saying where things stand: neither decides, because a database can
	// hold the highest version and still lack one below it.
	Applied  int64
	Required int64
	// FailedVersions are migrations recorded as attempted but not successful, which
	// is a distinct and worse situation than simply being behind: the database may
	// be half-migrated.
	FailedVersions []int64

	// missing are the versions this build has that the ledger does not record as applied, and
	// unknown the ones the ledger records that this build does not have. Both ascending.
	missing []int64
	unknown []int64
}

// Behind reports whether the database is positively known to lack a migration this
// build requires, below its highest applied version or above it.
func (s SchemaState) Behind() bool { return s.Determined && len(s.missing) > 0 }

// Ahead reports whether the database has migrations this build does not know about,
// which happens on a rollback.
func (s SchemaState) Ahead() bool { return s.Determined && len(s.unknown) > 0 }

// Missing lists exactly the migrations this build requires that are not applied, gaps
// below the highest applied version included, for a message an operator can act on
// without going digging.
func (s SchemaState) Missing() []int64 {
	if !s.Behind() {
		return nil
	}
	return append([]int64(nil), s.missing...)
}

// Unknown lists the applied migrations this build does not have.
func (s SchemaState) Unknown() []int64 {
	if !s.Ahead() {
		return nil
	}
	return append([]int64(nil), s.unknown...)
}

// compareSchema decides a state from what the ledger could be read to hold: the versions this
// build requires, the versions recorded as applied successfully, and those recorded as failed.
// A ledger that could not be read, or that records nothing successful (what a database
// mid-bootstrap looks like), is indeterminate. Pure.
func compareSchema(required, applied, failed []int64, readable bool) SchemaState {
	state := SchemaState{Required: highestOf(required)}
	if !readable || len(applied) == 0 {
		return state
	}
	state.Determined = true
	state.Applied = highestOf(applied)
	state.FailedVersions = sortedSet(failed)
	have := map[int64]bool{}
	for _, v := range applied {
		have[v] = true
	}
	need := map[int64]bool{}
	for _, v := range required {
		need[v] = true
		if !have[v] {
			state.missing = append(state.missing, v)
		}
	}
	for _, v := range applied {
		if !need[v] {
			state.unknown = append(state.unknown, v)
		}
	}
	state.missing = sortedSet(state.missing)
	state.unknown = sortedSet(state.unknown)
	return state
}

func highestOf(vs []int64) int64 {
	var h int64
	for _, v := range vs {
		if v > h {
			h = v
		}
	}
	return h
}

// sortedSet is vs ascending, each version once.
func sortedSet(vs []int64) []int64 {
	if len(vs) == 0 {
		return nil
	}
	out := append([]int64(nil), vs...)
	slices.Sort(out)
	return slices.Compact(out)
}

// InspectSchema reads the migration ledger and compares it with required. It returns a
// state rather than an error for the "cannot tell" cases, so the caller's policy stays
// in one place.
func InspectSchema(ctx context.Context, db *sql.DB, required []int64) SchemaState {
	if db == nil {
		return compareSchema(required, nil, nil, false)
	}

	// Does the ledger exist? A fresh database, or one migrated by another tool, is
	// indeterminate rather than behind.
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		sqlxMigrationsTable,
	).Scan(&exists); err != nil || !exists {
		return compareSchema(required, nil, nil, false)
	}

	// Every row: the applied versions are compared as a set, and a migration recorded as
	// unsuccessful means a previous run died partway, worth naming separately because
	// "run the migrations" is not the whole fix.
	rows, err := db.QueryContext(ctx, `SELECT version, success FROM `+sqlxMigrationsTable)
	if err != nil {
		return compareSchema(required, nil, nil, false)
	}
	defer rows.Close()
	var applied, failed []int64
	for rows.Next() {
		var v int64
		var ok bool
		if err := rows.Scan(&v, &ok); err != nil {
			return compareSchema(required, nil, nil, false)
		}
		if ok {
			applied = append(applied, v)
		} else {
			failed = append(failed, v)
		}
	}
	if rows.Err() != nil {
		return compareSchema(required, nil, nil, false)
	}
	return compareSchema(required, applied, failed, true)
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
		fmt.Fprintf(&b, "database schema is missing migrations this build requires: %s "+
			"(the highest applied is %d; this build's highest is %d). ",
			joinVersions(s.Missing()), s.Applied, s.Required)
		fmt.Fprintf(&b, "Apply them with `%s`, then restart this service. ", MigrateCommand())
		b.WriteString("Refusing to start: running against an older schema breaks durable agent runs, " +
			"silently discards admin AI settings in favour of env defaults, and returns 500s to users, " +
			"while the process looks healthy. Set ALLOW_SCHEMA_DRIFT=true to start anyway.")
		if s.Ahead() {
			fmt.Fprintf(&b, " The database also has migrations this build does not know about: %s.",
				joinVersions(s.Unknown()))
		}
	case s.Ahead():
		fmt.Fprintf(&b, "database schema is AHEAD of this build: it has migrations this build does not know about: %s "+
			"(the highest applied is %d; this build's highest is %d). ",
			joinVersions(s.Unknown()), s.Applied, s.Required)
		b.WriteString("Continuing, since this is what a rollback looks like, but columns this build does not " +
			"know about exist and a newer build may be expected.")
	default:
		b.WriteString("database schema version could not be determined; continuing without the check.")
	}
	return b.String()
}

// joinVersions lists versions for a person, with a run of three or more consecutive ones as a
// range ("205-213"), so a database far behind does not print two hundred numbers. Only
// versions that are consecutive integers join, so a number absent from the set is never
// implied. vs must be ascending.
func joinVersions(vs []int64) string {
	parts := make([]string, 0, len(vs))
	for i := 0; i < len(vs); {
		j := i
		for j+1 < len(vs) && vs[j+1] == vs[j]+1 {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, strconv.FormatInt(vs[i], 10)+"-"+strconv.FormatInt(vs[j], 10))
		} else {
			for k := i; k <= j; k++ {
				parts = append(parts, strconv.FormatInt(vs[k], 10))
			}
		}
		i = j + 1
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
	return schemaVerdict(InspectSchema(ctx, DBConn.SqlDB, RequiredMigrations), AllowSchemaDrift())
}

// schemaVerdict is VerifySchemaAtStartup's decision for a state: an error only when the
// database positively lacks a migration this build requires and the operator has not
// allowed drift; otherwise an advisory to log, empty when there is nothing to say. Pure.
func schemaVerdict(state SchemaState, allowDrift bool) (advisory string, err error) {
	switch {
	case state.Behind():
		msg := SchemaDriftMessage(state)
		if allowDrift {
			return "ALLOW_SCHEMA_DRIFT is set, starting anyway. " + msg, nil
		}
		return "", fmt.Errorf("%s", msg)
	case state.Ahead(), len(state.FailedVersions) > 0, !state.Determined:
		return SchemaDriftMessage(state), nil
	default:
		return "", nil
	}
}
