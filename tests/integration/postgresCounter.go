//go:build integration
// +build integration

package integration

// Counting the statements the server sends Postgres, for tests that hold a
// read to a fixed number of them however much it reads (WA2: the restricted
// task filter is one batched read per request, never one per task).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	gormPostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// PostgresCounter counts the statements sent through the server's pool.
type PostgresCounter struct{ n atomic.Int64 }

// Count is the statements sent since the last Reset.
func (c *PostgresCounter) Count() int { return int(c.n.Load()) }

// Reset starts the count again.
func (c *PostgresCounter) Reset() { c.n.Store(0) }

var registerCounting sync.Once

// countingDriverName is the driver CountPostgres registers: pgx's, counting.
const countingDriverName = "pgx-counting"

// counters maps a DSN to the counter its connections report to.
var counters sync.Map // string -> *PostgresCounter

// CountPostgres connects the server's pool (postgresInit.DBConn, the SQL and
// the gorm handle alike) to env's Postgres through a driver that counts every
// statement it sends: a query, an exec, a prepared statement's run. The pool
// is the server's for the rest of the test.
func CountPostgres(t *testing.T, env *Env) *PostgresCounter {
	t.Helper()
	registerCounting.Do(func() { sql.Register(countingDriverName, countingDriver{stdlib.GetDefaultDriver()}) })
	c := &PostgresCounter{}
	counters.Store(env.DSN, c)
	t.Cleanup(func() { counters.Delete(env.DSN) })
	db, err := sql.Open(countingDriverName, env.DSN)
	if err != nil {
		t.Fatalf("open a counting pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("reach Postgres through the counting pool: %v", err)
	}
	g, err := gorm.Open(gormPostgres.New(gormPostgres.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm over the counting pool: %v", err)
	}
	postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = db, g
	if postgresInit.DBConn.DBTimeout == 0 {
		postgresInit.DBConn.DBTimeout = 3e9
	}
	c.Reset()
	return c
}

type countingDriver struct{ inner driver.Driver }

func (d countingDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	var c *PostgresCounter
	if v, ok := counters.Load(dsn); ok {
		c = v.(*PostgresCounter)
	}
	return &countingConn{Conn: conn, c: c}, nil
}

type countingConn struct {
	driver.Conn
	c *PostgresCounter
}

func (c *countingConn) count() {
	if c.c != nil {
		c.c.n.Add(1)
	}
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.count()
	return q.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.count()
	return e.ExecContext(ctx, query, args)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var (
		s   driver.Stmt
		err error
	)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		s, err = p.PrepareContext(ctx, query)
	} else {
		s, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &countingStmt{Stmt: s, conn: c}, nil
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // the fallback database/sql itself uses
}

func (c *countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *countingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *countingConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type countingStmt struct {
	driver.Stmt
	conn *countingConn
}

func (s *countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.conn.count()
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		return e.ExecContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.Stmt.Exec(values) //nolint:staticcheck // the fallback database/sql itself uses
}

func (s *countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.conn.count()
	if q, ok := s.Stmt.(driver.StmtQueryContext); ok {
		return q.QueryContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.Stmt.Query(values) //nolint:staticcheck // the fallback database/sql itself uses
}

func (s *countingStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return s.conn.CheckNamedValue(nv)
}
