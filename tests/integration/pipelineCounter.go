//go:build integration
// +build integration

package integration

// Counting a request's store calls the way the 7th review check counts them:
// a round trip to Postgres is a statement, or a pipelined batch of them
// (pgexec.Batch) however many it holds; a graph call is a call. Only the
// measured call's own calls count: those made with a context from the
// counter's Context, or one made from it (a goroutine the request starts with
// its ctx, too), so work an earlier test left going is never counted.
//
// The pool is pgx's own driver with a tracer (pgx.QueryTracer and
// pgx.BatchTracer), not a wrapping database/sql driver, so pgexec.Batch
// still reaches pgx under database/sql (sql.Conn.Raw) and pipelines as it
// does in production. BEGIN and COMMIT both count: pgx's stdlib driver
// commits with the context BeginTx was given, the measured call's.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	gormPostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// StoreCalls is one measured call's store calls.
type StoreCalls struct {
	trips, statements, batches, graph atomic.Int64
	mu                                sync.Mutex
	sent                              []string
	args                              [][]any
}

type storeCallsKey struct{}

// Context is parent marked as the measured call's.
func (c *StoreCalls) Context(parent context.Context) context.Context {
	return context.WithValue(parent, storeCallsKey{}, c)
}

// Trips is the Postgres round trips: statements outside a batch, plus one per batch.
func (c *StoreCalls) Trips() int { return int(c.trips.Load()) }

// Statements is every statement, a batch's each counted.
func (c *StoreCalls) Statements() int { return int(c.statements.Load()) }

// Batches is the pipelined batches among the trips.
func (c *StoreCalls) Batches() int { return int(c.batches.Load()) }

// Graph is the Dgraph calls.
func (c *StoreCalls) Graph() int { return int(c.graph.Load()) }

// Sent is how many statements sent held every one of words.
func (c *StoreCalls) Sent(words ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sent {
		all := true
		for _, w := range words {
			all = all && strings.Contains(s, w)
		}
		if all {
			n++
		}
	}
	return n
}

// Said is every statement sent, in order, each on one line and cut to 150
// characters: what a test lists when a call breaks its budget.
func (c *StoreCalls) Said() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sent))
	for _, s := range c.sent {
		out = append(out, said(s))
	}
	return out
}

// said is a statement on one line, cut to 150 characters.
func said(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 150 {
		s = s[:150]
	}
	return s
}

// Reset starts the count again.
func (c *StoreCalls) Reset() {
	c.trips.Store(0)
	c.statements.Store(0)
	c.batches.Store(0)
	c.graph.Store(0)
	c.mu.Lock()
	c.sent, c.args = nil, nil
	c.mu.Unlock()
}

func (c *StoreCalls) note(q string, args []any) {
	c.mu.Lock()
	c.sent = append(c.sent, q)
	c.args = append(c.args, append([]any(nil), args...))
	c.mu.Unlock()
}

// Named is how many statements sent held every one of words and named id by
// an argument of its own: a read of that one table or row by its id, where a
// read of a set of them names it inside a list (NamedIn). It matches on the
// arguments, not on the text, so a statement is the thing read whatever its
// SQL says.
func (c *StoreCalls) Named(id string, words ...string) int {
	return c.naming(id, false, words)
}

// NamedIn is Named that also counts a statement naming id inside a list
// argument (a uuid[], an ANY($1)).
func (c *StoreCalls) NamedIn(id string, words ...string) int {
	return c.naming(id, true, words)
}

// Naming is every statement whose arguments name id, alone or in a list,
// each on one line and cut to 150 characters, as Said.
func (c *StoreCalls) Naming(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for i, s := range c.sent {
		if namesID(c.args[i], id, true) {
			out = append(out, said(s))
		}
	}
	return out
}

func (c *StoreCalls) naming(id string, inLists bool, words []string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i, s := range c.sent {
		all := namesID(c.args[i], id, inLists)
		for _, w := range words {
			all = all && strings.Contains(s, w)
		}
		if all {
			n++
		}
	}
	return n
}

// namesID is whether a statement's arguments name id: one argument equal to
// it, or, inLists, a list argument holding it (a Go slice, or a driver value
// written out as Postgres's {a,b} array text).
func namesID(args []any, id string, inLists bool) bool {
	for _, a := range args {
		if argIs(a, id) {
			return true
		}
		if inLists && argHolds(a, id) {
			return true
		}
	}
	return false
}

func argIs(a any, id string) bool {
	if rv := reflect.ValueOf(a); !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return false
	}
	switch v := a.(type) {
	case string:
		return v == id
	case fmt.Stringer:
		return v.String() == id
	}
	return false
}

func argHolds(a any, id string) bool {
	if a == nil {
		return false
	}
	if v, ok := a.(driver.Valuer); ok {
		if out, err := v.Value(); err == nil {
			if s, ok := out.(string); ok && strings.HasPrefix(s, "{") {
				return strings.Contains(s, id)
			}
		}
	}
	rv := reflect.ValueOf(a)
	if rv.Kind() != reflect.Slice || rv.Type().Elem().Kind() == reflect.Uint8 {
		return false
	}
	for i := range rv.Len() {
		if argIs(rv.Index(i).Interface(), id) {
			return true
		}
	}
	return false
}

func storeCallsOf(ctx context.Context) *StoreCalls {
	c, _ := ctx.Value(storeCallsKey{}).(*StoreCalls)
	return c
}

type storeCallsTracer struct{}

func (storeCallsTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if c := storeCallsOf(ctx); c != nil {
		c.trips.Add(1)
		c.statements.Add(1)
		c.note(data.SQL, data.Args)
	}
	return ctx
}

func (storeCallsTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (storeCallsTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if c := storeCallsOf(ctx); c != nil && data.Batch != nil {
		c.trips.Add(1)
		c.batches.Add(1)
		c.statements.Add(int64(data.Batch.Len()))
		for _, q := range data.Batch.QueuedQueries {
			c.note(q.SQL, q.Arguments)
		}
	}
	return ctx
}

func (storeCallsTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (storeCallsTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// CountStoreCalls points the server's pool (the SQL and gorm handles) at env's
// Postgres through pgx with the counting tracer, and the server's graph client
// at graphEndpoint through a connection that counts, until t ends.
func CountStoreCalls(t *testing.T, env *Env, graphEndpoint string) *StoreCalls {
	t.Helper()
	c := &StoreCalls{}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatalf("the counting pool's config: %v", err)
	}
	cfg.Tracer = storeCallsTracer{}
	db := stdlib.OpenDB(*cfg)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("reach Postgres through the counting pool: %v", err)
	}
	g, err := gorm.Open(gormPostgres.New(gormPostgres.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm over the counting pool: %v", err)
	}
	liveSQL, liveGorm := postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB
	postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = db, g
	markConnected(t)
	if postgresInit.DBConn.DBTimeout == 0 {
		postgresInit.DBConn.DBTimeout = 3e9
	}
	count := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if m := storeCallsOf(ctx); m != nil {
			m.graph.Add(1)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	conn, err := grpc.NewClient(graphEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(count), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	if err != nil {
		t.Fatalf("a counting graph client: %v", err)
	}
	liveGraph := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))
	t.Cleanup(func() {
		dgraphInit.DgraphClient = liveGraph
		postgresInit.DBConn.SqlDB, postgresInit.DBConn.GormDB = liveSQL, liveGorm
		_ = conn.Close()
		_ = db.Close()
	})
	return c
}
