package postgresInit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A refused statement's kind is its first caller outside the database layers,
// without the module's path: never pgx's, database/sql's, gorm's, pgexec's or
// this package's frames, whose names say nothing of who ran it. A package
// named like a layer but not one (pgexec_test) is a caller.
func TestAStatementsKindIsItsFirstCallerOutsideTheDatabaseLayers(t *testing.T) {
	stack := []string{
		"runtime.Callers",
		module + "initializers/postgresInit.callerKind",
		module + "initializers/postgresInit.spaceTracer.TraceQueryEnd",
		"github.com/jackc/pgx/v5.(*baseRows).Close",
		"github.com/jackc/pgx/v5/stdlib.(*Rows).Next",
		"database/sql.(*Rows).nextLocked",
		"gorm.io/gorm.(*DB).Scan",
		module + "models/pgexec.queryOnce[...]",
		module + "models/postgres/DataTable.patchRowsTx.func1",
		module + "business/DataTable.(*Business).PatchRow",
	}
	if k := firstCaller(stack); k != "models/postgres/DataTable.patchRowsTx.func1" {
		t.Errorf("kind %q; want models/postgres/DataTable.patchRowsTx.func1", k)
	}
	if k := firstCaller([]string{"database/sql.(*Rows).Next", module + "models/pgexec_test.refusedThroughPgexec"}); k != "models/pgexec_test.refusedThroughPgexec" {
		t.Errorf("kind %q; want models/pgexec_test.refusedThroughPgexec", k)
	}
	if k := firstCaller(stack[:8]); k != "an unnamed caller" {
		t.Errorf("a stack of database layers alone: kind %q; want an unnamed caller", k)
	}
}

// A kind is said once a minute, with how many of it came since its last
// line, however often it is refused. Kinds are callers, so few; past the cap
// a new kind is still said, never dropped unsaid, and never kept (V-589).
func TestARefusedKindIsSaidOnceAMinuteWithHowManyCame(t *testing.T) {
	r := &refusals{kinds: map[string]*refusalsOfKind{}}
	at := time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC)
	steps := []struct {
		kind  string
		after time.Duration
		say   bool
		n     int
	}{
		{"a", 0, true, 1},
		{"a", 10 * time.Second, false, 0},
		{"b", 20 * time.Second, true, 1},
		{"a", 30 * time.Second, false, 0},
		{"a", 60 * time.Second, true, 3},
		{"a", 61 * time.Second, false, 0},
	}
	for i, s := range steps {
		say, n, total := r.count(s.kind, at.Add(s.after))
		if say != s.say || n != s.n || total != int64(i+1) {
			t.Errorf("step %d (%s at +%s): say %v n %d total %d; want %v %d %d", i, s.kind, s.after, say, n, total, s.say, s.n, i+1)
		}
	}

	full := &refusals{kinds: map[string]*refusalsOfKind{}}
	for i := 0; i < kindsKept; i++ {
		full.count(strings.Repeat("k", i+1), at)
	}
	for i := 0; i < 2; i++ {
		if say, n, _ := full.count("new", at.Add(time.Second)); !say || n != 1 {
			t.Errorf("a new kind past the cap, refusal %d: say %v n %d; want said each time", i, say, n)
		}
	}
	if len(full.kinds) != kindsKept {
		t.Errorf("%d kinds kept; want the cap, %d", len(full.kinds), kindsKept)
	}
	if say, _, _ := full.count("later", at.Add(time.Minute)); !say || len(full.kinds) != 1 {
		t.Errorf("a minute on, a new kind: said %v, %d kinds kept; want said, the stale ones let go", say, len(full.kinds))
	}
}

// The line names /dev/shm only when a parallel query's shared memory ran
// out; 53100 is a full disk's code too (V-589).
func TestSpaceAdviceNamesSharedMemoryOnlyForASegment(t *testing.T) {
	shm := &pgconn.PgError{Code: "53100", Message: `could not resize shared memory segment "/PostgreSQL.1" to 8388608 bytes: No space left on device`}
	disk := &pgconn.PgError{Code: "53100", Message: `could not write to file "base/pgsql_tmp/pgsql_tmp1.0": No space left on device`}
	if a := spaceAdvice(shm); !strings.Contains(a, "/dev/shm") {
		t.Errorf("shared memory: %q; want /dev/shm named", a)
	}
	if a := spaceAdvice(disk); strings.Contains(a, "/dev/shm") || !strings.Contains(a, "disk") {
		t.Errorf("a full disk: %q; want its disk named, not /dev/shm", a)
	}
}

// Every session carries the tracer, and it counts a refusal for space only:
// a statement that succeeds, or fails otherwise, costs a comparison.
func TestEverySessionNamesStatementsRefusedForSpace(t *testing.T) {
	cfg, err := sessionConfig("postgres://u@localhost:5432/db")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tracer.(spaceTracer); !ok {
		t.Fatalf("a session's tracer is %T; want spaceTracer", cfg.Tracer)
	}
	before := refusedForSpace.total.Load()
	ctx := context.Background()
	for _, err := range []error{nil, errors.New("connection reset"), &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}} {
		spaceTracer{}.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: err})
	}
	if n := refusedForSpace.total.Load() - before; n != 0 {
		t.Errorf("%d refusals counted for statements not refused for space; want none", n)
	}
}
