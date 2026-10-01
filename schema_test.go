package onecamp

import (
	"os"
	"testing"
)

// The embedded migration set must match the directory it was built from, FILE FOR FILE.
//
// WHY COUNT AND NOT JUST THE MAXIMUM. The obvious check is that the embedded highest version equals the
// highest on disk, and it is not enough: a glob can match a subset and still include the newest file.
// `migrations/1*.up.sql` would capture 1, 10-19 and 100-140 — a third of the set missing, with the
// ceiling unchanged at 140 and every max-based assertion passing. Comparing counts is what actually
// pins the pattern.
//
// This lives in the root package because that is the only place both halves are visible: the embedded
// FS is an unexported var here, and the directory is readable from the test's working directory. The
// alternative was exporting a count accessor purely for a test in another package, which would then be
// an exported function nothing in production calls — the thing helpers/deadExportedFuncGuard_test.go
// exists to catch.
func TestEmbeddedMigrationsMatchTheDirectory(t *testing.T) {
	embedded, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("cannot read the embedded migrations: %v", err)
	}

	onDisk, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("cannot read the migrations directory: %v", err)
	}

	countUp := func(names []os.DirEntry) (int, int64) {
		n := 0
		var highest int64
		for _, e := range names {
			groups := migrationFileRe.FindStringSubmatch(e.Name())
			if groups == nil {
				continue
			}
			n++
			var v int64
			for _, digit := range groups[1] {
				v = v*10 + int64(digit-'0')
			}
			if v > highest {
				highest = v
			}
		}
		return n, highest
	}

	embeddedCount, embeddedHighest := countUp(embedded)
	diskCount, diskHighest := countUp(onDisk)

	if diskCount == 0 {
		t.Fatal("no up-migrations found on disk — this test is looking in the wrong place and is " +
			"enforcing nothing")
	}
	if embeddedCount != diskCount {
		t.Errorf("the go:embed pattern captured %d up-migration(s) but %d exist on disk. A partial "+
			"match can still contain the newest file, so the version ceiling would look correct while "+
			"the set is incomplete. Check the //go:embed line in schema.go.",
			embeddedCount, diskCount)
	}
	if embeddedHighest != diskHighest {
		t.Errorf("embedded ceiling is %d but the directory's is %d", embeddedHighest, diskHighest)
	}
	if HighestMigration() != diskHighest {
		t.Errorf("HighestMigration() reports %d, the directory says %d", HighestMigration(), diskHighest)
	}
}

// A file that does not follow the convention must be ignored rather than lowering the ceiling.
//
// sqlx-cli requires "<version>_<description>.up.sql", so anything else in that directory is not a
// migration — a README, an editor backup, a stray .sql without a version. The risk is not that such a
// file is skipped; it is that a loose parser reads it as version 0 and drags the maximum down, which
// would disable the drift guard for every database at or above the real ceiling.
func TestUnconventionalFilenamesAreIgnored(t *testing.T) {
	for _, name := range []string{
		"README.md",
		"notes.sql",
		"140_add_thing.down.sql", // a down-migration: right version, wrong direction
		"_scratch.up.sql",
		"v140_add_thing.up.sql", // prefixed, so not a bare version
	} {
		if migrationFileRe.MatchString(name) {
			t.Errorf("%q should not be treated as an up-migration", name)
		}
	}

	// And the shapes that must match, including a single digit and a three-digit version.
	for _, name := range []string{
		"9_create_users_chat_notification_table.up.sql",
		"99_add_ai_pii_redaction.up.sql",
		"140_add_scim_provisioning.up.sql",
	} {
		if !migrationFileRe.MatchString(name) {
			t.Errorf("%q is a real migration filename and must match", name)
		}
	}
}
