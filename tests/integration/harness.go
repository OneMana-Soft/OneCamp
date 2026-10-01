//go:build integration
// +build integration

// Package integration provides a shared testcontainers-based harness
// for tests that need a real Postgres + the project's migrations
// applied.
//
// Why the build tag: the unit test job (`go test ./...`) must not pull
// in this package, because (a) it shells out to Docker via
// testcontainers-go which isn't available on every developer's box, and
// (b) it pulls in the migrate library which would otherwise be a
// heavyweight transitive dep on the unit set. CI's integration job runs
// `go test -tags=integration` to opt in.
//
// Usage from a test file (also under //go:build integration):
//
//	func TestSomething(t *testing.T) {
//	    env := integration.SetupEnv(t)
//	    defer env.Close()
//	    // env.PG is a *sql.DB pointed at a fresh Postgres with
//	    // migrations applied. env.DSN gives you the URL for libraries
//	    // that want it.
//	}
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Env carries the live test environment for a single test. Call Close
// to release the container at the end.
type Env struct {
	t         *testing.T
	container testcontainers.Container
	PG        *sql.DB
	DSN       string
}

// SetupEnv brings up a Postgres 12 container, applies every migration
// under ../../migrations, and returns a connected *sql.DB. The
// container is automatically removed at the end of the test via t.Cleanup.
//
// Each call gives you a fresh database, so two tests in the same
// package don't share state.
func SetupEnv(t *testing.T) *Env {
	t.Helper()
	// Required before any test wires the project's own pool at this container.
	//
	// postgresInit.ConnectPostgres logs on its SUCCESS path via helpers.MessageLogs,
	// which loggerInit populates in the server and nothing populates in a test binary —
	// so connecting panics on a nil *log.Logger before returning. A test would crash
	// rather than fail, with a stack pointing at the logger instead of at the cause.
	//
	// Done here rather than in each test because every test that talks to the model
	// layer needs it, and the same guard already exists in SetupDgraph for the same
	// reason.
	ensureLoggers()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image: "postgres:12-alpine",
		Env: map[string]string{
			"POSTGRES_USER":     "onecamp",
			"POSTGRES_PASSWORD": "onecamp",
			"POSTGRES_DB":       "onecamp_test",
		},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	dsn := fmt.Sprintf("postgres://onecamp:onecamp@%s:%s/onecamp_test?sslmode=disable",
		host, port.Port())

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Connection pool is small for tests so we don't blow past the
	// default postgres max_connections of 100 if tests run in parallel.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(2 * time.Minute)

	// Wait for SQL-level readiness (the log-level wait is necessary
	// but not sufficient; pg startup logs appear before the listener
	// is fully up).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.Ping(); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("postgres did not become reachable: %v", err)
	}

	// Pre-create the uuid_generate_v4 extension that several migrations
	// depend on. Production deploys do this in their bootstrap script.
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`); err != nil {
		t.Fatalf("create extension uuid-ossp: %v", err)
	}
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		t.Fatalf("create extension pgcrypto: %v", err)
	}

	if err := applyMigrations(dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	env := &Env{t: t, container: c, PG: db, DSN: dsn}
	t.Cleanup(env.Close)
	return env
}

// Close terminates the container and closes the DB pool.
func (e *Env) Close() {
	if e == nil {
		return
	}
	if e.PG != nil {
		_ = e.PG.Close()
	}
	if e.container != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.container.Terminate(ctx)
	}
}

// applyMigrations walks the project's migrations/ directory and applies
// every up.sql to the freshly-created database.
//
// The path resolution is "relative to this file at compile time", so
// running tests from any working directory still finds the migrations.
func applyMigrations(dsn string) error {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("unable to resolve caller path")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		return fmt.Errorf("migrate.New: %w", err)
	}
	defer func() {
		// Ignore the close errors — the DB is going away with the
		// container anyway.
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
