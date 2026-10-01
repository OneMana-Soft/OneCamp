package postgresInit

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// schemaVersion_test.go — the constant must never drift from the migrations on
// disk, and the policy must never turn "cannot tell" into "refuse to start".

var migrationFileRe = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// highestMigrationOnDisk reads the repository's migrations directory.
func highestMigrationOnDisk(t *testing.T) int64 {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", dir, err)
	}
	var highest int64
	count := 0
	for _, e := range entries {
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		count++
		v, perr := strconv.ParseInt(m[1], 10, 64)
		if perr != nil {
			t.Fatalf("unparseable migration version in %q", e.Name())
		}
		if v > highest {
			highest = v
		}
	}
	if count == 0 {
		t.Fatal("found no migrations; the test is looking in the wrong place")
	}
	return highest
}

// THE DRIFT GUARD, now guarding something different.
//
// RequiredSchemaVersion used to be a hand-maintained constant, and this test existed so that adding a
// migration without bumping it broke the build instead of silently weakening the check. The value is
// now derived from the embedded migration set (see package onecamp), so that particular mistake is no
// longer possible to make.
//
// The test is kept because it still asserts something the derivation cannot assert about itself: that
// the number this package will compare a live database against agrees with the migrations actually
// present in the repository, read INDEPENDENTLY, from a different package, through the filesystem rather
// than through the embed. If the go:embed pattern ever stops matching the directory, the two disagree
// here. TestEmbeddedMigrationsMatchTheDirectory covers the same seam from the other side by comparing
// file counts, which is the case a maximum-only comparison cannot see.
func TestRequiredSchemaVersionMatchesMigrations(t *testing.T) {
	onDisk := highestMigrationOnDisk(t)
	if RequiredSchemaVersion != onDisk {
		t.Fatalf("RequiredSchemaVersion is %d but the highest migration on disk is %d.\n"+
			"Bump the constant in initializers/postgresInit/schemaVersion.go to %d — otherwise a deploy "+
			"can run against a database missing migration %d and the startup check will not notice.",
			RequiredSchemaVersion, onDisk, onDisk, onDisk)
	}
}

func TestSchemaState_Classification(t *testing.T) {
	behind := SchemaState{Determined: true, Applied: 132, Required: 137}
	if !behind.Behind() || behind.Ahead() {
		t.Fatalf("132 vs 137 must be behind: %+v", behind)
	}
	level := SchemaState{Determined: true, Applied: 137, Required: 137}
	if level.Behind() || level.Ahead() {
		t.Fatalf("equal versions are neither behind nor ahead: %+v", level)
	}
	ahead := SchemaState{Determined: true, Applied: 140, Required: 137}
	if !ahead.Ahead() || ahead.Behind() {
		t.Fatalf("140 vs 137 must be ahead: %+v", ahead)
	}
}

// THE SAFETY PROPERTY. A check that cannot read the ledger must never be reported
// as "behind", because the consequence of that mistake is refusing to boot a
// perfectly healthy server.
func TestSchemaState_UndeterminedIsNeverBehind(t *testing.T) {
	undetermined := SchemaState{Determined: false, Applied: 0, Required: 137}
	if undetermined.Behind() {
		t.Fatal("an indeterminate check must not be treated as behind — that would refuse to start a healthy server")
	}
	if undetermined.Ahead() {
		t.Fatal("an indeterminate check must not be treated as ahead either")
	}
	if len(undetermined.Missing()) != 0 {
		t.Fatal("an indeterminate check has nothing to report as missing")
	}
}

func TestSchemaState_MissingListsTheExactGap(t *testing.T) {
	s := SchemaState{Determined: true, Applied: 132, Required: 137}
	got := s.Missing()
	want := []int64{133, 134, 135, 136, 137}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
	// A level database has no gap.
	if gap := (SchemaState{Determined: true, Applied: 137, Required: 137}).Missing(); len(gap) != 0 {
		t.Fatalf("a level database has no missing migrations, got %v", gap)
	}
}

// The message is the whole value of failing fast: it has to name the gap and the
// command, or an operator is no better off than with the original error loop.
func TestSchemaDriftMessage_IsActionable(t *testing.T) {
	msg := SchemaDriftMessage(SchemaState{Determined: true, Applied: 132, Required: 137})
	for _, want := range []string{"132", "137", "133, 134, 135, 136, 137", "make migrate_server_up", "ALLOW_SCHEMA_DRIFT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q so it can be acted on; got:\n%s", want, msg)
		}
	}
}

// A half-finished migration run is a different problem from simply being behind,
// and saying so saves an operator from "I ran it and it still fails".
func TestSchemaDriftMessage_NamesFailedMigrations(t *testing.T) {
	msg := SchemaDriftMessage(SchemaState{
		Determined: true, Applied: 132, Required: 137, FailedVersions: []int64{133},
	})
	if !strings.Contains(msg, "FAILED") || !strings.Contains(msg, "133") {
		t.Fatalf("a failed migration must be called out distinctly; got:\n%s", msg)
	}
}

// A rollback must not be blocked.
func TestSchemaDriftMessage_AheadContinues(t *testing.T) {
	msg := SchemaDriftMessage(SchemaState{Determined: true, Applied: 140, Required: 137})
	if !strings.Contains(msg, "AHEAD") || !strings.Contains(msg, "Continuing") {
		t.Fatalf("a database ahead of the build must warn and continue; got:\n%s", msg)
	}
	if strings.Contains(msg, "Refusing to start") {
		t.Fatal("being ahead must not refuse to start — that would block a rollback")
	}
}

func TestAllowSchemaDrift(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "1", " true "} {
		t.Setenv("ALLOW_SCHEMA_DRIFT", v)
		if !AllowSchemaDrift() {
			t.Errorf("%q should enable the override", v)
		}
	}
	for _, v := range []string{"", "false", "0", "yes-please", "maybe"} {
		t.Setenv("ALLOW_SCHEMA_DRIFT", v)
		if AllowSchemaDrift() {
			t.Errorf("%q must not enable the override", v)
		}
	}
}
