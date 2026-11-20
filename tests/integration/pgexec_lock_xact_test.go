//go:build integration
// +build integration

package integration_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	bookingDomain "github.com/akashc777/OneCamp/domain/Booking"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	slaModel "github.com/akashc777/OneCamp/models/postgres/SLA"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
	urgentPingModel "github.com/akashc777/OneCamp/models/postgres/UrgentPing"
	"github.com/akashc777/OneCamp/tests/integration"
)

// pgexec.LockXact holds its lock to the end of the transaction ctx carries
// (InTx, or a *sql.Tx handed in with WithTx), and refuses outside one. Another
// session tries the same key while it is held and after.
func TestLockXactHoldsItsLockToTheEndOfItsTransaction(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	key := "h8.lockxact:" + uuid.NewString()
	// free tries the lock from another session; a try in its own statement
	// lets go of it at once when it gets it.
	free := func() bool {
		var got bool
		if err := env.PG.QueryRow(`SELECT pg_try_advisory_xact_lock(hashtext($1))`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := pgexec.LockXact(ctx, key); !errors.Is(err, pgexec.ErrNotInTx) {
		t.Fatalf("LockXact outside a transaction = %v, want ErrNotInTx", err)
	}

	t.Run("inside InTx it is held to the commit", func(t *testing.T) {
		err := pgexec.InTx(ctx, func(ctx context.Context) error {
			if err := pgexec.LockXact(ctx, key); err != nil {
				return err
			}
			if free() {
				t.Error("another session took the lock while the transaction held it")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !free() {
			t.Error("the lock outlived its transaction")
		}
	})

	t.Run("a transaction handed in with WithTx holds it to its rollback", func(t *testing.T) {
		tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := pgexec.LockXact(pgexec.WithTx(ctx, tx), key); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if free() {
			t.Error("another session took the lock while the handed-in transaction held it")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if !free() {
			t.Error("the lock outlived its rolled-back transaction")
		}
	})
}

// Each lock moved onto LockXact keeps the key value it had: taken through
// LockXact with the key its site takes (the site's own exported key), the
// lock is the one the last release's statement asked for, so while a replica
// of the last release and one of this release run side by side they still
// wait on each other. The old statements are frozen here as they shipped; the
// keys come from the sites. Every LockXact call in production Go has its case
// (lockXactCallers), so a new one can't go without.
func TestLockXactKeysAreTheLocksTheyReplaced(t *testing.T) {
	ctx := context.Background()
	project, other, id := uuid.New(), uuid.New(), uuid.New()
	type oldLock struct {
		sql  string
		args []any
	}
	cases := []struct {
		site, name string
		take       func(ctx context.Context) error
		old        []oldLock
	}{
		{"models/postgres/Goal/goalModel.go", "SetParent's goal tree", lockXactTakes(goalModel.TreeLockKey),
			[]oldLock{{`hashtext('onecamp.goals.tree')`, nil}}},
		{"models/postgres/TaskChange/taskChangeModel.go", "Append's task changes", lockXactTakes(taskChangeModels.ChangesLockKey),
			[]oldLock{{`hashtext('task_changes')`, nil}}},
		{"models/postgres/UrgentPing/urgentPingModel.go", "Claim's sender and recipient", lockXactTakes(urgentPingModel.PairLockKey(project, other)),
			[]oldLock{{`hashtext('urgent_ping:' || $1::text || ':' || $2::text)`, []any{project, other}}}},
		{"models/postgres/DataTable/computedModel.go", "LockKept's table (a batch)", lockXactTakes(dataTableModel.KeptLockKey(id)),
			[]oldLock{{`hashtextextended('onecamp.tables.kept:' || $1::text, 0)`, []any{id}}}},
		{"models/postgres/DataTable/computedModel.go", "LockKept's table (a reader)", lockXactTakes(dataTableModel.KeptLockKey(id)),
			[]oldLock{{`hashtextextended('onecamp.tables.kept:' || $1::text, 0)`, []any{id}}}},
		{"models/postgres/AdminAudit/chainWriter.go", "the audit chain", lockXactTakes(auditModel.AuditChainLockKey),
			[]oldLock{{`472074::bigint`, nil}}},
		{"models/postgres/Booking/bookingModel.go", "Reserve's page", lockXactTakes(bookingDomain.PageLockKey(id)),
			[]oldLock{{`hashtext('booking:' || $1::text)`, []any{id}}}},
		{"models/postgres/Booking/bookingModel.go", "HoldPage's page", lockXactTakes(bookingDomain.PageLockKey(id)),
			[]oldLock{{`hashtext('booking:' || $1::text)`, []any{id}}}},
		{"models/postgres/SLA/slaModel.go", "a project's SLA rules", lockXactTakes(slaModel.RulesLockKey(project)),
			[]oldLock{{`hashtext('sla_rules:' || $1::text)`, []any{project}}}},
		{"models/postgres/SoftDelete/softDelete.go", "Restore's bounds (an SLA rule's)",
			lockXactTakes(softDelete.LockKeys([]softDelete.Bound{{Lock: slaModel.RulesLockKey(project)}})...),
			[]oldLock{{`hashtext('sla_rules:' || $1::text)`, []any{project}}}},
		{"models/postgres/DocFile/docFileModel.go", "RecordVersion's doc", lockXactTakes(docFileModels.VersionsLockKey(id)),
			[]oldLock{{`hashtext('doc_file_versions:' || $1::text)`, []any{id}}}},
		{"models/postgres/Milestone/milestoneModel.go", "a project's milestones and a milestone's projects",
			lockXactTakes(milestoneModel.ProjectLockKey(project), milestoneModel.MilestoneLockKey(id)),
			[]oldLock{{`hashtext('milestones:' || $1::text)`, []any{project}}, {`hashtext('milestone:' || $1::text)`, []any{id}}}},
		{"models/postgres/Release/releaseGit.go", "ShipTag's project", lockXactTakes(releaseModel.ProjectLockKey(project)),
			[]oldLock{{`hashtext('releases:' || $1::text)`, []any{project}}}},
		{"models/postgres/Cycle/cycleModel.go", "Create's project (a cycle)", lockXactTakes(cycleModel.ProjectLockKey(project)),
			[]oldLock{{`hashtext('cycles:' || $1::text)`, []any{project}}}},
		{"models/postgres/Cycle/cycleModel.go", "lockProject's project", lockXactTakes(cycleModel.ProjectLockKey(project)),
			[]oldLock{{`hashtext('cycles:' || $1::text)`, []any{project}}}},
		{"models/pgexec/advisory.go", "Capped's key", func(ctx context.Context) error {
			_, err := pgexec.Capped(ctx, "onecamp.capped:"+id.String(), 1,
				func(context.Context) (int, error) { return 0, nil }, func(context.Context) error { return nil })
			return err
		}, []oldLock{{`hashtext($1)`, []any{"onecamp.capped:" + id.String()}}}},
	}

	calls := lockXactCallers(t)
	cased := map[string]int{}
	for _, c := range cases {
		cased[c.site]++
	}
	for site, n := range calls {
		if cased[site] != n {
			t.Errorf("%s has %d LockXact calls and %d cases here: give each call a case taking the key from its site", site, n, cased[site])
		}
	}
	for site, n := range cased {
		if calls[site] == 0 {
			t.Errorf("%d cases here name %s, which has no LockXact call: drop them", n, site)
		}
	}

	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		tx, err := env.PG.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.take(pgexec.WithTx(ctx, tx)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s (%s): %v", c.name, c.site, err)
		}
		for _, o := range c.old {
			var free bool
			if err := env.PG.QueryRow(`SELECT pg_try_advisory_xact_lock(`+o.sql+`)`, o.args...).Scan(&free); err != nil {
				_ = tx.Rollback()
				t.Fatalf("%s (%s): %v", c.name, c.site, err)
			}
			if free {
				t.Errorf("%s (%s): the last release's lock %s was free while LockXact held the site's key: the key changed", c.name, c.site, o.sql)
			}
		}
		_ = tx.Rollback()
	}
}

// lockXactTakes takes keys, in order, through LockXact.
func lockXactTakes[K string | pgexec.WideKey | int64](keys ...K) func(context.Context) error {
	return func(ctx context.Context) error {
		for _, k := range keys {
			if err := pgexec.LockXact(ctx, k); err != nil {
				return err
			}
		}
		return nil
	}
}

// lockXactCallers counts the LockXact calls in production Go (not tests,
// testdata or comments) by file, from the repository's root.
func lockXactCallers(t *testing.T) map[string]int {
	t.Helper()
	calls := map[string]int{}
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "../.." && (strings.HasPrefix(name, ".") || name == "tests" || name == "testdata" || name == "vendor" || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") && strings.Contains(line, "LockXact(") {
				calls[filepath.ToSlash(strings.TrimPrefix(path, "../../"))]++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("no LockXact call found from ../..: the walk read nothing")
	}
	return calls
}
