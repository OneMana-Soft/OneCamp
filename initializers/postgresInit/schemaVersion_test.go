package postgresInit

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// schemaVersion_test.go — the constant must never drift from the migrations on
// disk, and the policy must never turn "cannot tell" into "refuse to start".

var migrationFileRe = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// migrationsOnDisk reads the repository's migrations directory: every up-migration's version,
// ascending, each once.
func migrationsOnDisk(t *testing.T) []int64 {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", dir, err)
	}
	var versions []int64
	for _, e := range entries {
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, perr := strconv.ParseInt(m[1], 10, 64)
		if perr != nil {
			t.Fatalf("unparseable migration version in %q", e.Name())
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		t.Fatal("found no migrations; the test is looking in the wrong place")
	}
	return sortedSet(versions)
}

// THE DRIFT GUARD, now guarding something different.
//
// The required version used to be a hand-maintained constant, and this test existed so that adding a
// migration without bumping it broke the build instead of silently weakening the check. The set is
// now derived from the embedded migrations (see package onecamp), so that particular mistake is no
// longer possible to make.
//
// The test is kept because it still asserts something the derivation cannot assert about itself: that
// the set this package will compare a live database against is the set of migrations actually present
// in the repository, read INDEPENDENTLY, from a different package, through the filesystem rather than
// through the embed. If the go:embed pattern ever stops matching the directory, the two disagree here.
func TestRequiredMigrationsMatchTheDirectory(t *testing.T) {
	if onDisk := migrationsOnDisk(t); !slices.Equal(RequiredMigrations, onDisk) {
		t.Fatalf("this build would compare a database against %d migration(s) ending %v, but the "+
			"directory holds %d ending %v; a deploy could then run against a database missing one "+
			"and the startup check would not notice",
			len(RequiredMigrations), tail(RequiredMigrations), len(onDisk), tail(onDisk))
	}
}

// tail is the last few versions of vs, for a failure message.
func tail(vs []int64) []int64 {
	if len(vs) > 5 {
		return vs[len(vs)-5:]
	}
	return vs
}

// span is the versions from a to b, inclusive.
func span(a, b int64) []int64 {
	var out []int64
	for v := a; v <= b; v++ {
		out = append(out, v)
	}
	return out
}

func TestSchemaState_Classification(t *testing.T) {
	behind := compareSchema(span(1, 137), span(1, 132), nil, true)
	if !behind.Behind() || behind.Ahead() {
		t.Fatalf("132 of 137 applied must be behind: %+v", behind)
	}
	level := compareSchema(span(1, 137), span(1, 137), nil, true)
	if level.Behind() || level.Ahead() {
		t.Fatalf("the same set is neither behind nor ahead: %+v", level)
	}
	ahead := compareSchema(span(1, 137), span(1, 140), nil, true)
	if !ahead.Ahead() || ahead.Behind() {
		t.Fatalf("140 applied against a build of 137 must be ahead: %+v", ahead)
	}
}

// THE CASE THE MAXIMUM COULD NOT SEE. A migration numbered above work that has not shipped (a
// hotfix's 214 while 205-213 wait on beta), or one in a programme's reserved range, is applied first.
// The highest applied version then equals this build's highest, and a check by maximum passed a
// database missing nine migrations. The set comparison names exactly those, and refuses to start.
func TestSchemaState_AGapBelowTheHighestIsBehind(t *testing.T) {
	applied := append(span(1, 204), 214)
	s := compareSchema(span(1, 214), applied, nil, true)
	if !s.Behind() {
		t.Fatalf("a database missing 205-213 must be behind, though it holds 214: %+v", s)
	}
	if got, want := s.Missing(), span(205, 213); !slices.Equal(got, want) {
		t.Fatalf("Missing() = %v, want exactly %v", got, want)
	}
	if s.Applied != 214 || s.Required != 214 {
		t.Fatalf("the highest applied and required are both 214: %+v", s)
	}
	advisory, err := schemaVerdict(s, false)
	if err == nil {
		t.Fatalf("it must refuse to start; got advisory %q", advisory)
	}
	for _, want := range []string{"205-213", "Refusing to start", "ALLOW_SCHEMA_DRIFT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q; got:\n%s", want, err)
		}
	}
	// The operator's way through is unchanged.
	advisory, err = schemaVerdict(s, true)
	if err != nil || !strings.Contains(advisory, "ALLOW_SCHEMA_DRIFT is set") {
		t.Fatalf("ALLOW_SCHEMA_DRIFT must start anyway and say so; got %q, %v", advisory, err)
	}
}

// A rollback: the database has migrations this build does not know. It warns, names them, and starts.
func TestSchemaState_ARollbackWarnsAndContinues(t *testing.T) {
	s := compareSchema(span(1, 210), span(1, 214), nil, true)
	if !s.Ahead() || s.Behind() {
		t.Fatalf("214 applied against a build of 210 is ahead and not behind: %+v", s)
	}
	if got, want := s.Unknown(), span(211, 214); !slices.Equal(got, want) {
		t.Fatalf("Unknown() = %v, want %v", got, want)
	}
	advisory, err := schemaVerdict(s, false)
	if err != nil {
		t.Fatalf("being ahead must not refuse to start — that would block a rollback: %v", err)
	}
	for _, want := range []string{"AHEAD", "Continuing", "211-214"} {
		if !strings.Contains(advisory, want) {
			t.Errorf("the warning must mention %q; got:\n%s", want, advisory)
		}
	}
}

// A migration recorded as failed is not applied: it is missing, so the server refuses, and the
// message says it FAILED, which "run the migrations" alone would not fix.
func TestSchemaState_AFailedVersionIsMissingAndNamed(t *testing.T) {
	s := compareSchema(span(1, 214), span(1, 213), []int64{214}, true)
	if !s.Behind() {
		t.Fatalf("a failed 214 is not applied: %+v", s)
	}
	if got := s.Missing(); !slices.Equal(got, []int64{214}) {
		t.Fatalf("Missing() = %v, want [214]", got)
	}
	if !slices.Equal(s.FailedVersions, []int64{214}) {
		t.Fatalf("FailedVersions = %v, want [214]", s.FailedVersions)
	}
	_, err := schemaVerdict(s, false)
	if err == nil || !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "214") {
		t.Fatalf("a failed migration must refuse and be called out distinctly; got %v", err)
	}
}

// THE SAFETY PROPERTY. A check that cannot read the ledger must never be reported as "behind",
// because the consequence of that mistake is refusing to boot a perfectly healthy server. It warns,
// and continues.
func TestSchemaState_AnIndeterminateLedgerWarnsAndContinues(t *testing.T) {
	for name, s := range map[string]SchemaState{
		"no database":                    InspectSchema(context.Background(), nil, span(1, 214)),
		"a ledger that cannot be read":   compareSchema(span(1, 214), nil, nil, false),
		"a ledger with nothing applied":  compareSchema(span(1, 214), nil, []int64{1}, true),
		"a ledger with nothing recorded": compareSchema(span(1, 214), nil, nil, true),
	} {
		if s.Determined || s.Behind() || s.Ahead() || len(s.Missing()) != 0 {
			t.Errorf("%s: an indeterminate check is neither behind nor ahead, and has nothing missing: %+v", name, s)
		}
		advisory, err := schemaVerdict(s, false)
		if err != nil {
			t.Errorf("%s: must never refuse to start: %v", name, err)
		}
		if !strings.Contains(advisory, "could not be determined") {
			t.Errorf("%s: must warn that the check could not run; got %q", name, advisory)
		}
	}
}

// A level database starts without a word.
func TestSchemaState_ALevelDatabaseStartsQuietly(t *testing.T) {
	advisory, err := schemaVerdict(compareSchema(span(1, 214), span(1, 214), nil, true), false)
	if err != nil || advisory != "" {
		t.Fatalf("a level database has nothing to say; got %q, %v", advisory, err)
	}
}

// The message is the whole value of failing fast: it has to name the gap and the
// command, or an operator is no better off than with the original error loop.
func TestSchemaDriftMessage_IsActionable(t *testing.T) {
	msg := SchemaDriftMessage(compareSchema(span(1, 137), span(1, 132), nil, true))
	for _, want := range []string{"132", "137", "133-137", "make migrate_server_up", "ALLOW_SCHEMA_DRIFT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q so it can be acted on; got:\n%s", want, msg)
		}
	}
}

// Versions are listed for a person: runs of three or more as a range, and never a range across a
// number that is not in the set.
func TestJoinVersions(t *testing.T) {
	for _, c := range []struct {
		in   []int64
		want string
	}{
		{span(205, 213), "205-213"},
		{[]int64{197, 200, 201}, "197, 200, 201"},
		{[]int64{1, 2}, "1, 2"},
		{append(span(205, 213), 340, 343), "205-213, 340, 343"},
		{nil, ""},
	} {
		if got := joinVersions(c.in); got != c.want {
			t.Errorf("joinVersions(%v) = %q, want %q", c.in, got, c.want)
		}
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
