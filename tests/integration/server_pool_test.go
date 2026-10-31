//go:build integration
// +build integration

package integration

// A test that reads Postgres without connecting the server's pool fails, every
// time, saying so (noPostgres, harness.go): whatever the test before it did
// with the pool, and when no test before it did anything.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/taskread"
)

func TestAReadWithoutConnectingPostgresSaysSo(t *testing.T) {
	for _, before := range []struct {
		name string
		did  func(t *testing.T)
	}{
		{"after a test that connected the pool", func(t *testing.T) { ConnectServerPostgres(t) }},
		{"after a test that connected it itself", func(t *testing.T) {
			env := SetupEnv(t)
			if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
				t.Fatalf("connect the server's pool: %v", err)
			}
		}},
		{"after a test that counted its statements", func(t *testing.T) { CountPostgres(t, ConnectServerPostgres(t)) }},
	} {
		t.Run(before.name, func(t *testing.T) {
			// Its database is dropped when it ends, as a test's is.
			t.Run("the test before", before.did)
			saysNeverConnected(t)
		})
	}

	t.Run("before any test has connected it", func(t *testing.T) {
		sqlDB, gormDB := postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB
		t.Cleanup(func() { postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = sqlDB, gormDB })
		// As a new binary has it.
		postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = nil, nil
		t.Run("a test that sets up only the graph", func(t *testing.T) {
			guardTestState(t) // what every Setup does first
			saysNeverConnected(t)
		})
	})

	t.Run("closing another of the test's databases leaves the connected one", func(t *testing.T) {
		ConnectServerPostgres(t)
		SetupEnv(t).Close()
		if _, err := taskread.For(context.Background(), uuid.NewString()); err != nil {
			t.Errorf("a task read once another database of the test's closed: %v; want the connected one still read", err)
		}
	})
}

// saysNeverConnected reads Postgres the two ways the server does, through
// models/taskread (pgexec, on the SQL pool) and through the gorm handle, and
// wants each to fail saying the test never connected the pool.
func saysNeverConnected(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	const says = "never called ConnectServerPostgres"
	if _, err := taskread.For(ctx, uuid.NewString()); err == nil || !strings.Contains(err.Error(), says) {
		t.Errorf("a task read (models/taskread) answered %v; want it to fail saying the test %s", err, says)
	}
	g := postgresInit.DBConn.GormDB
	if g == nil {
		t.Errorf("the server's gorm handle is nil; want one that fails saying the test %s", says)
		return
	}
	var n int
	if err := g.WithContext(ctx).Raw(`SELECT 1`).Scan(&n).Error; err == nil || !strings.Contains(err.Error(), says) {
		t.Errorf("a read through gorm answered %v; want it to fail saying the test %s", err, says)
	}
}

// Every session of the server's pool runs without JIT compiling. Postgres
// compiles a statement whose cost passes jit_above_cost (100,000) before it
// reads a row: a table's sums over 100k rows spent 1.5-1.8 s compiling on
// the demo's half-CPU Postgres, past their 2 s budget, and its sorted
// windows ran out of time beside them (V-114). A DSN that sets jit keeps it.
func TestEverySessionOfTheServersPoolRunsWithoutJIT(t *testing.T) {
	env := ConnectServerPostgres(t)
	for i, jit := range sessionsJIT(t, 3) {
		if jit != "off" {
			t.Errorf("session %d of the server's pool runs with jit %s; want off", i+1, jit)
		}
	}
	var jit string
	if err := postgresInit.DBConn.GormDB.WithContext(context.Background()).Raw(`SHOW jit`).Scan(&jit).Error; err != nil {
		t.Fatalf("SHOW jit through the server's gorm handle: %v", err)
	}
	if jit != "off" {
		t.Errorf("a session of the server's gorm handle runs with jit %s; want off", jit)
	}

	t.Run("a DSN that sets jit keeps it", func(t *testing.T) {
		if err := postgresInit.ConnectPostgres(context.Background(), env.DSN+"&jit=on"); err != nil {
			t.Fatalf("connect the server's pool with jit=on in its DSN: %v", err)
		}
		for i, jit := range sessionsJIT(t, 2) {
			if jit != "on" {
				t.Errorf("session %d of a pool whose DSN says jit=on runs with jit %s; want on", i+1, jit)
			}
		}
	})
}

// sessionsJIT is SHOW jit on n sessions of the server's pool held at once,
// so each is a session of its own, never one handed back and taken again.
func sessionsJIT(t *testing.T, n int) []string {
	t.Helper()
	ctx := context.Background()
	jits := make([]string, n)
	for i := range jits {
		c, err := postgresInit.DBConn.SqlDB.Conn(ctx)
		if err != nil {
			t.Fatalf("session %d of the server's pool: %v", i+1, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.QueryRowContext(ctx, `SHOW jit`).Scan(&jits[i]); err != nil {
			t.Fatalf("SHOW jit on session %d: %v", i+1, err)
		}
	}
	return jits
}
