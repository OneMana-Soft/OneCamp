// Package onecamp holds what the BINARY needs to know about the repository it was built from.
//
// It exists for one reason: the running server has no migrations directory. The container ships a
// compiled binary, and migrations/ is bind-mounted only into the throwaway rust:alpine container that
// runs sqlx. So anything the server needs to know about the migration set has to be baked in at build
// time. That build-time/runtime asymmetry is the real constraint — not, as the comment in
// initializers/postgresInit/schemaVersion.go used to say, a go:embed limitation.
//
// WHY THE ROOT OF THE MODULE. go:embed patterns may not contain "..", so a package cannot reach
// UPWARD to migrations/. They can reach downward freely, and this package sits above it. The
// alternative — a .go file inside migrations/ — was rejected: sqlx-cli reads that directory, and
// putting a foreign file in it to satisfy the Go toolchain trades a small problem for a risk to the
// one tool that must never break.
//
// The cost is roughly 600 KB of SQL in the binary, which is noise next to a Go runtime, and it buys
// something beyond the version number: the migration set now travels WITH the binary that requires it.
// The class of failure that took a tenant offline for weeks — provisioning copied migrations once, so
// a database could never reach the schema its own binary demanded — becomes impossible to reproduce
// once files are applied from here rather than from whatever happens to be on disk. That step is not
// taken yet; this is the half that makes it available.
package onecamp

import (
	"embed"
	"fmt"
	"regexp"
	"strconv"
)

// migrationFiles is every up-migration, compiled in.
//
// A glob that matches NOTHING is a compile error in Go, which is the property that makes this safe:
// there is no silent path to an empty set. Down-migrations are deliberately excluded — the version
// ceiling is defined by what must be applied, and embedding rollbacks would double the payload to say
// the same thing.
//
//go:embed migrations/*.up.sql
var migrationFiles embed.FS

// migrationFileRe matches "<version>_<description>.up.sql", the shape sqlx-cli requires.
//
// Kept identical to the regexp in postgresInit's drift test on purpose. That test reads the DIRECTORY
// while this reads the EMBED, so the two agreeing is what proves the glob above actually captured the
// files on disk — a check that survives precisely because the two halves are computed independently.
var migrationFileRe = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// highestMigration is resolved once, at init.
var highestMigration = mustComputeHighestMigration()

// HighestMigration returns the highest migration version this binary was built with.
//
// This is what the server compares the database against. It is derived rather than hand-maintained
// because the previous constant had to be bumped by hand on every migration, and the only thing
// standing between forgetting and a silently-disabled schema check was a test remembering to fail.
func HighestMigration() int64 { return highestMigration }

// mustComputeHighestMigration scans the embedded set, and PANICS if it cannot find a version.
//
// Panicking in an init path is a deliberate choice, and the direction of the failure is the reason.
// Returning 0 would leave the server comparing its schema against version zero, so `applied < required`
// would be false for every database and the drift guard would be off — silently, on a process that
// reports itself healthy. That is the exact failure the guard was written to end. A build whose
// embedded migrations cannot be parsed is broken in a way no deployment should paper over, so it fails
// at startup with the reason instead.
func mustComputeHighestMigration() int64 {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		panic(fmt.Sprintf("onecamp: cannot read embedded migrations: %v", err))
	}

	var highest int64
	matched := 0
	for _, entry := range entries {
		groups := migrationFileRe.FindStringSubmatch(entry.Name())
		if groups == nil {
			continue
		}
		version, parseErr := strconv.ParseInt(groups[1], 10, 64)
		if parseErr != nil {
			// Unreachable while the regexp requires \d+, and asserted rather than ignored: a silent
			// skip here would lower the ceiling and weaken the guard by exactly one migration.
			panic(fmt.Sprintf("onecamp: unparseable migration version in %q: %v", entry.Name(), parseErr))
		}
		matched++
		if version > highest {
			highest = version
		}
	}

	if matched == 0 || highest == 0 {
		panic(fmt.Sprintf("onecamp: embedded %d file(s) but found no usable migration version; "+
			"the go:embed pattern or the filename convention has changed", len(entries)))
	}
	return highest
}
