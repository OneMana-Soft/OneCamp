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
// `go test -tags=integration` to opt in. main.go, which reaches no Docker,
// is the one file outside the tag (it says why).
//
// Usage from a test file (also under //go:build integration), in a package
// whose TestMain runs the tests through Main (main.go):
//
//	func TestMain(m *testing.M) { os.Exit(integration.Main(m)) }
//
//	func TestSomething(t *testing.T) {
//	    env := integration.SetupEnv(t)
//	    defer env.Close()
//	    // env.PG is a *sql.DB pointed at a fresh database with
//	    // migrations applied. env.DSN gives you the URL for libraries
//	    // that want it.
//	}
package integration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	gormPostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// Env carries the live test environment for a single test: a database of
// its own. Call Close (or let t.Cleanup) to drop it at the end.
type Env struct {
	t        *testing.T
	server   *pgServer
	database string
	closed   sync.Once
	PG       *sql.DB
	DSN      string
}

// SetupEnv gives the test a Postgres 12 database of its own with every
// migration under ../../migrations applied, and returns a connected *sql.DB.
// The database is dropped at the end of the test via t.Cleanup.
//
// Each call gives you a fresh database, so two tests in the same package
// don't share state. The databases are copies of one template the binary
// migrated once, from an empty database, on the binary's one Postgres
// (shared.go): a copy holds what a new container with the migrations applied
// held, and takes well under a second instead of half a minute. A test that
// applies migrations itself (migratesItself) gets an empty database with
// every migration applied to it in the test, as every test's database was
// before.
func SetupEnv(t *testing.T) *Env {
	t.Helper()
	newDB := (*pgServer).newDatabase
	if migratesItself[strings.SplitN(t.Name(), "/", 2)[0]] {
		newDB = (*pgServer).newMigratedDatabase
	}
	return setupEnv(t, newDB)
}

// SetupUnmigratedEnv is SetupEnv with a database no migration has reached:
// the extensions production's bootstrap creates and nothing else, what a
// fresh install's server first connects to. MigrateAsTheInstallDoes migrates
// it.
func SetupUnmigratedEnv(t *testing.T) *Env {
	t.Helper()
	return setupEnv(t, (*pgServer).newBlankDatabase)
}

// setupEnv is SetupEnv with the test's database made by newDB.
func setupEnv(t *testing.T, newDB func(*pgServer) (string, error)) *Env {
	t.Helper()
	guardTestState(t)
	// Required before any test wires the project's own pool at this database.
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

	var name string
	srv, err := thePostgres.prepared(startPostgres, func(s *pgServer) (err error) {
		if name, err = newDB(s); err != nil {
			return fmt.Errorf("make the test's database: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	env := &Env{t: t, server: srv, database: name, DSN: srv.dsn(name)}
	t.Cleanup(env.Close)

	db, err := sql.Open("postgres", env.DSN)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	env.PG = db

	// Connection pool is small for tests so we don't blow past the
	// default postgres max_connections of 100 if tests run in parallel.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(2 * time.Minute)

	if err := pingUntil(db, 30*time.Second); err != nil {
		t.Fatalf("postgres did not become reachable: %v", err)
	}

	settleBeforeTeardown(t)
	return env
}

// ConnectServerPostgres is SetupEnv with the server's own pool connected to
// it, as the server connects at boot: for a test of code that reads Postgres
// as it goes. Every read of tasks does since restricted tasks (WA2: who may
// see one is decided from its rows, models/taskread). A test of task reads
// that sets up only the graph reads a pool that reaches nothing, and every
// statement fails saying the test never called this (noPostgres, below).
func ConnectServerPostgres(t *testing.T) *Env {
	t.Helper()
	env := SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("connect the server's pool: %v", err)
	}
	markConnected(t)
	return env
}

// Close drops the test's database and closes the DB pool. Sessions still
// open on it (the project's own pool, which the test pointed here, among
// them) are ended first, as the test's own container going ended them. The
// server's pool, once it reaches no database, points at nothing (noPostgres).
func (e *Env) Close() {
	if e == nil {
		return
	}
	e.closed.Do(func() {
		if e.PG != nil {
			_ = e.PG.Close()
		}
		if e.server != nil && e.database != "" {
			if err := e.server.dropDatabase(e.database); err != nil {
				e.t.Logf("drop the test's database %s: %v", e.database, err)
			}
			releaseServerPostgres()
		}
	})
}

// errNeverConnected is what every statement on the server's pool answers
// while it points at nothing.
var errNeverConnected = errors.New("this test reads Postgres (for example through models/taskread) but never called ConnectServerPostgres, so the server's pool reaches no database")

// noPostgres is the server's pool (postgresInit.DBConn, the SQL and the gorm
// handle alike) when no test has it connected: every connection it tries
// fails with errNeverConnected, so every statement does.
//
// A pool used to stay where the last test to connect it left it, on a
// database dropped when that test ended. A test that read Postgres without
// connecting (a search's or a profile's task reads, through models/taskread,
// since WA2) failed with "database test_140 does not exist", and only after
// such a test: run alone or first, it read a nil pool, which the models read
// as no rows, and passed. Now it fails every time, saying what to do. The pool
// points here from a test's first Setup until the test connects it, and again
// once the database it reached is dropped (Close). It is never nil once a
// test has used the harness, so a goroutine a test left behind gets an error,
// not a nil dereference (mqtt.go). Production's pool is untouched: this is
// only ever set here.
var noPostgres struct {
	once sync.Once
	sql  *sql.DB
	gorm *gorm.DB
}

// noPostgresReached counts the connections tried on noPostgres. Code that
// fails closed on a read error (a refusal, a search answering 400) leaves the
// test failing on something else, so a test that fails having tried one is
// told why as well (guardTestState).
var noPostgresReached atomic.Int64

// neverConnected is noPostgres's connector and driver.
type neverConnected struct{}

func (neverConnected) Connect(context.Context) (driver.Conn, error) {
	noPostgresReached.Add(1)
	return nil, errNeverConnected
}
func (c neverConnected) Driver() driver.Driver { return c }
func (c neverConnected) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}

// pointServerPostgresAtNothing makes the server's pool noPostgres.
func pointServerPostgresAtNothing() {
	noPostgres.once.Do(func() {
		noPostgres.sql = sql.OpenDB(neverConnected{})
		// No ping: there is nothing to reach, and gorm opens no connection
		// of its own over a pool it is given.
		g, err := gorm.Open(gormPostgres.New(gormPostgres.Config{Conn: noPostgres.sql}), &gorm.Config{DisableAutomaticPing: true})
		if err != nil {
			panic(fmt.Sprintf("integration: gorm over a pool that reaches nothing: %v", err))
		}
		noPostgres.gorm = g
	})
	if postgresInit.DBConn == nil {
		postgresInit.DBConn = &postgresInit.DB{}
	}
	postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = noPostgres.sql, noPostgres.gorm
	if postgresInit.DBConn.DBTimeout <= 0 {
		// What ConnectPostgres sets: a statement given no time at all would
		// fail on its deadline before it said why.
		postgresInit.DBConn.DBTimeout = 3 * time.Second
	}
}

// serverPostgresNeverNil points a server's pool no test has connected yet at
// nothing. Every Setup calls it (guardTestState): a test that runs first, or
// alone, fails the way one that runs after another does.
func serverPostgresNeverNil() {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		pointServerPostgresAtNothing()
	}
}

// releaseServerPostgres points the server's pool at nothing once it reaches
// no database: the one a test connected it to has been dropped, or a pool a
// test opened over it (CountPostgres) closed. A pool that still reaches one,
// another database the test holds, is left as it is.
func releaseServerPostgres() {
	p := postgresInit.DBConn
	if p == nil || p.SqlDB == nil || p.SqlDB == noPostgres.sql {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p.SqlDB.PingContext(ctx) == nil {
		return
	}
	gone := p.SqlDB
	pointServerPostgresAtNothing()
	_ = gone.Close()
}

// pgServer is the binary's Postgres: the template the tests' databases are
// copied from, a connection to the maintenance database to make and drop
// them, the databases tests hold now, and the server's own settings as it
// started (teststate.go).
type pgServer struct {
	container  testcontainers.Container
	host, port string
	admin      *sql.DB
	made       atomic.Int64
	liveMu     sync.Mutex
	live       map[string]bool
	baseline   map[string]string
}

// migratesItself names the tests that apply migrations themselves: down, up,
// or to a version, with golang-migrate or by running a migration's SQL. Each
// gets an empty database (CREATE DATABASE … TEMPLATE template0) with every
// migration applied to it by golang-migrate in the test, not a copy of the
// template, so what it does to migrations starts from a database migrated in
// its own run, as before the shared server.
// TestTheTestsThatApplyMigrationsGetADatabaseMigratedFromBlank reads the test
// files and fails when this list and theirs differ.
var migratesItself = map[string]bool{
	"TestChangeSetOperationsMigrationDownUp":                    true,
	"TestContentIOJobsWaitReadyForAChoice":                      true,
	"TestComputedStoreMigrationUpDownUp":                        true,
	"TestApprovalsAnyItemMigrationUpDownUp":                     true,
	"TestBookingPages":                                          true,
	"TestCalendarAttendanceMigrationUpDownUp":                   true,
	"TestCalendarContainersMigrationUpDownUp":                   true,
	"TestCalendarFeedsMigrationUpDownUp":                        true,
	"TestCalendarImportMigrationUpDownUp":                       true,
	"TestCallStateMigrationUpDownUp":                            true,
	"TestCallAdmissionsMigrationUpDownUp":                       true,
	"TestCallRoomsMigrationUpDownUp":                            true,
	"TestCallHandsMigrationUpDownUp":                            true,
	"TestCallLobbyMigrationUpDownUp":                            true,
	"TestCallLobbyInviteesMigrationUpDownUp":                    true,
	"TestCallQualityMigrationUpDownUp":                          true,
	"TestCallATakesOnePostOnce":                                 true,
	"TestCallHoldsMigrationUpDownUp":                            true,
	"TestCallActivitiesMigrationUpDownUp":                       true,
	"TestCallBoardMigrationUpDownUp":                            true,
	"TestCallRecordingByMigrationUpDownUp":                      true,
	"TestCalendarProposalsMigrationUpDownUp":                    true,
	"TestCalendarRemindersMigrationUpDownUp":                    true,
	"TestCalendarStoreMigrationUpDownUp":                        true,
	"TestCalendarSubscriptionsMigrationUpDownUp":                true,
	"TestCalendarSyncMigrationUpDownUp":                         true,
	"TestCalendarSyncRefusalsMigrationUpDownUp":                 true,
	"TestCalendarTaskBlocksMigrationUpDownUp":                   true,
	"TestConnectedWork":                                         true,
	"TestContentIOMigrationUpDownUp":                            true,
	"TestConversationsMigrationUpDownUp":                        true,
	"TestDocAIIndexMigrationUpDownUp":                           true,
	"TestDocAccessResolver":                                     true,
	"TestDocFileVersionsMigrationUpDownUp":                      true,
	"TestDocExportConsentsMigrationUpDownUp":                    true,
	"TestDocTreeAcceptance":                                     true,
	"TestDocsClientLoopMigration":                               true,
	"TestDocVersionsMigrationUpDownUp":                          true,
	"TestEditProposalsMigrationUpDownUp":                        true,
	"TestEmailLookupIndexSurvivesAddressesThatDifferOnlyInCase": true,
	"TestEarlierSlackImportsMessagesAreKeyedByConversation":     true,
	"TestImportAccessReviewsMigrationUpDownUp":                  true,
	"TestInvitationLinksMigrationUpDownUp":                      true,
	"TestMigration204ClosesInvitationsToMembers":                true,
	"TestOutboxMigrationUpDownUp":                               true,
	"TestPersonPreferencesOverHTTP":                             true,
	"TestPublishMigrationUpDownUp":                              true,
	"TestResourceFollowsMigrationUpDownUp":                      true,
	"TestResourceMentionSyncsMigrationUpDownUp":                 true,
	"TestSoftDeleteMigrationUpDownUp":                           true,
	"TestTableMigrationFieldSettings":                           true,
	"TestTableMigrationRowLedger":                               true,
	"TestTableRowComments":                                      true,
	"TestTableRowPages":                                         true,
	"TestThreadsMigrationUpDownUp":                              true,
	"TestWebhookTokensMigrationUpDownUp":                        true,
}

var thePostgres shared[pgServer]

// templateDB holds every migration, applied once. Nothing connects to it
// afterwards: CREATE DATABASE copies only a template nobody is using.
const templateDB = "onecamp_template"

func (s *pgServer) dsn(database string) string {
	return fmt.Sprintf("postgres://onecamp:onecamp@%s:%s/%s?sslmode=disable", s.host, s.port, database)
}

// startPostgres starts the binary's Postgres and makes the template: the
// extensions production's bootstrap creates, then every migration.
func startPostgres() (*pgServer, func(), error) {
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:12-alpine",
			Env: map[string]string{
				"POSTGRES_USER":     "onecamp",
				"POSTGRES_PASSWORD": "onecamp",
				"POSTGRES_DB":       "onecamp_test",
			},
			// Flushing to disk is for surviving a crash, and no test crashes
			// the server. Without it, copying the template and migrating it
			// take a fraction of the time; what a session reads is the same.
			Cmd:          []string{"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"},
			ExposedPorts: []string{"5432/tcp"},
			// One start a run, so a slow one on a busy machine (37 s at worst
			// in the kept logs) is given room; a stuck one still fails.
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start postgres container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("container host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432")
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("container port: %w", err)
	}
	s := &pgServer{container: c, host: host, port: port.Port()}

	admin, err := sql.Open("postgres", s.dsn("postgres"))
	if err != nil {
		terminate(c)
		return nil, nil, fmt.Errorf("open the maintenance database: %w", err)
	}
	admin.SetMaxOpenConns(2)
	s.admin = admin
	stop := func() {
		_ = admin.Close()
		terminate(c)
	}
	// Wait for SQL-level readiness (the log-level wait is necessary
	// but not sufficient; pg startup logs appear before the listener
	// is fully up).
	if err := pingUntil(admin, 30*time.Second); err != nil {
		stop()
		return nil, nil, fmt.Errorf("postgres did not become reachable: %w", err)
	}

	if _, err := admin.Exec(`CREATE DATABASE ` + templateDB); err != nil {
		stop()
		return nil, nil, fmt.Errorf("create the template database: %w", err)
	}
	tmpl, err := sql.Open("postgres", s.dsn(templateDB))
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("open the template database: %w", err)
	}
	// Pre-create the uuid_generate_v4 extension that several migrations
	// depend on. Production deploys do this in their bootstrap script.
	for _, ext := range []string{`"uuid-ossp"`, `pgcrypto`} {
		if _, err := tmpl.Exec(`CREATE EXTENSION IF NOT EXISTS ` + ext); err != nil {
			_ = tmpl.Close()
			stop()
			return nil, nil, fmt.Errorf("create extension %s: %w", ext, err)
		}
	}
	_ = tmpl.Close()
	began := time.Now()
	if err := applyMigrations(s.dsn(templateDB)); err != nil {
		stop()
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}
	// Said in the run's output: the run's from-scratch proof of the
	// migrations is this, every one applied to an empty Postgres 12.
	fmt.Printf("integration: every migration applied to an empty database (the template) in %s\n", time.Since(began).Round(time.Millisecond))
	s.live = map[string]bool{}
	if s.baseline, err = s.pgSettings(); err != nil {
		stop()
		return nil, nil, fmt.Errorf("read postgres's own settings: %w", err)
	}
	return s, stop, nil
}

// newDatabase copies the template into a database of its own and answers
// its name. Postgres waits up to 5 s for the template's last session to end
// before refusing a copy, and the copy is tried again for half a minute.
func (s *pgServer) newDatabase() (string, error) {
	name := fmt.Sprintf("test_%d", s.made.Add(1))
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := s.admin.Exec(`CREATE DATABASE ` + name + ` TEMPLATE ` + templateDB)
		if err == nil {
			s.held(name, true)
			return name, nil
		}
		if time.Now().After(deadline) {
			return name, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// newMigratedDatabase makes a blank database and applies every migration to
// it.
func (s *pgServer) newMigratedDatabase() (string, error) {
	name, err := s.newBlankDatabase()
	if err != nil {
		return name, err
	}
	if err := applyMigrations(s.dsn(name)); err != nil {
		return name, fmt.Errorf("apply migrations: %w", err)
	}
	return name, nil
}

// newBlankDatabase makes an empty database with the extensions production's
// bootstrap creates, and nothing else.
func (s *pgServer) newBlankDatabase() (string, error) {
	name := fmt.Sprintf("test_%d", s.made.Add(1))
	if _, err := s.admin.Exec(`CREATE DATABASE ` + name + ` TEMPLATE template0`); err != nil {
		return name, err
	}
	s.held(name, true)
	db, err := sql.Open("postgres", s.dsn(name))
	if err != nil {
		return name, err
	}
	defer func() { _ = db.Close() }()
	for _, ext := range []string{`"uuid-ossp"`, `pgcrypto`} {
		if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS ` + ext); err != nil {
			return name, fmt.Errorf("create extension %s: %w", ext, err)
		}
	}
	return name, nil
}

// MigrateAsTheInstallDoes applies every migration to the test's database as
// `make migrate_up` does on an install: each migration's SQL (with
// golang-migrate, as every test database is migrated), and sqlx-cli's ledger
// recording each version as applied, which is what the server's schema check
// reads (postgresInit.InspectSchema).
func (e *Env) MigrateAsTheInstallDoes(t *testing.T) {
	t.Helper()
	if err := applyMigrations(e.DSN); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := e.PG.Exec(`CREATE TABLE _sqlx_migrations (version BIGINT PRIMARY KEY, description TEXT NOT NULL,
		installed_on TIMESTAMPTZ NOT NULL DEFAULT now(), success BOOLEAN NOT NULL, checksum BYTEA NOT NULL,
		execution_time BIGINT NOT NULL)`); err != nil {
		t.Fatalf("sqlx's ledger: %v", err)
	}
	if _, err := e.PG.Exec(`INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time)
		SELECT v, 'migration', true, '\x00', 0 FROM unnest($1::bigint[]) AS v`, pq.Array(postgresInit.RequiredMigrations)); err != nil {
		t.Fatalf("sqlx's ledger rows: %v", err)
	}
}

// held records that a test holds the database, or no longer does.
func (s *pgServer) held(name string, yes bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if yes {
		s.live[name] = true
	} else {
		delete(s.live, name)
	}
}

// dropDatabase drops a test's database: first no new sessions, then the
// open ones ended, since Postgres 12 drops only a database nobody is in.
func (s *pgServer) dropDatabase(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.admin.ExecContext(ctx, `ALTER DATABASE `+name+` ALLOW_CONNECTIONS false`)
	for {
		_, _ = s.admin.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		_, err := s.admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name)
		if err == nil || ctx.Err() != nil {
			// No test holds it now: one still there is left behind, and the
			// test's guard says so (teststate.go).
			s.held(name, false)
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pingUntil pings db until it answers or wait has passed.
func pingUntil(db *sql.DB, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := db.Ping(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return db.Ping()
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
		// Ignore the close errors — the template is copied, never used
		// directly, once they're applied.
		_, _ = m.Close()
	}()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
