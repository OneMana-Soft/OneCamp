// Package generation keeps the rows that decide which tasks a reader may see
// (models/taskread: task_restrictions and their grants, triage_items) in the
// process for as long as they can't have changed: they change rarely, and
// every request that lists tasks reads them.
//
// Their generation (registry.TaskReadGeneration) is read with the profiles
// in VerifyAuth's one MGET and carried on the request's ctx (With). Every
// write to those rows calls Bump: after its commit the shared generation
// moves, for every process, and this process's own count moves at once. A
// Cache entry serves a request only while both are what the entry was filled
// at, on the same database, and for at most MaxAge.
//
// A stale entry would show a task newly restricted, or newly waiting in
// triage, to people it now hides from (and keep one let in hidden). It can
// only outlive a write made in another process whose bump was lost, Redis
// failing writes but not reads, and for at most MaxAge. A request whose MGET
// failed carries no generation it can trust: it reads the rows and empties
// the Cache. Reads off a request (jobs, services/AI's seenOnly, token and
// OAuth requests) carry none and read the rows as before.
//
// Only a read is served from a Cache: a ctx marked Cacheable (a GET or HEAD
// request; middleware.RequestIDMiddleware marks it). Everything else reads
// the rows themselves (MAIN, 17 Nov: writes read fresh): every other method,
// MQTT handlers, workers, the outbox, agents' and bots' tool calls, and any
// path a new write takes. So a write is never let in by a stale entry, and a
// grant revoked or a restriction added stops writes at once; reads may be up
// to MaxAge stale, as above.
package generation

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// MaxAge bounds an entry whatever the generation: the longest a write whose
// bump was lost stays unseen (Redis failing writes but not reads, or a
// long-lived request still carrying the generation it started with). A
// reader pays the decision's reads again at most once per MaxAge.
const MaxAge = 15 * time.Second

// maxKeys caps a Cache's entries (one per reader): past it, it starts over.
const maxKeys = 10_000

// bumps counts this process's committed writes to the rows.
var bumps atomic.Int64

// heldAt, when set, is the time entries age by instead of the clock's: only
// HoldClockForTest, in the integration build, sets it.
var heldAt atomic.Pointer[time.Time]

func now() time.Time {
	if t := heldAt.Load(); t != nil {
		return *t
	}
	return time.Now()
}

type ctxKey struct{}

type read struct {
	n  int64
	ok bool
}

// With is ctx carrying the generation read for its request: n, when ok is
// true; ok is false when Redis failed the read.
func With(ctx context.Context, n int64, ok bool) context.Context {
	return context.WithValue(ctx, ctxKey{}, read{n: n, ok: ok})
}

type cacheableKey struct{}

// Cacheable is ctx marked as a read a Cache may serve (a GET or HEAD
// request). An unmarked ctx always reads the rows.
func Cacheable(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheableKey{}, true)
}

// IsCacheable is whether ctx is marked a read a Cache may serve.
func IsCacheable(ctx context.Context) bool {
	cacheable, _ := ctx.Value(cacheableKey{}).(bool)
	return cacheable
}

// Uncacheable is ctx no longer marked a read: the work a read sets off that
// may write (taskread.Detached). It reads the rows.
func Uncacheable(ctx context.Context) context.Context {
	return context.WithValue(ctx, cacheableKey{}, false)
}

// Bump marks ctx's write to the rows. After the outermost commit (never,
// when it rolls back) this process's caches stop serving what they hold and
// the shared generation moves; a failed move logs a WARN naming writer.
func Bump(ctx context.Context, writer string) {
	pgexec.AfterCommit(ctx, func(ctx context.Context) {
		bumps.Add(1)
		_ = redisStore.BumpGeneration(context.WithoutCancel(ctx), registry.TaskReadGeneration, writer)
	})
}

// Cache is one read of the rows, kept by key (a reader, or "" for a read
// that is everyone's).
type Cache[T any] struct {
	entries *helpers.TTLCache[entry[T]]
}

type entry[T any] struct {
	gen, bumps int64
	db         *sql.DB
	v          T
}

// NewCache is an empty Cache.
func NewCache[T any]() *Cache[T] {
	return &Cache[T]{entries: helpers.NewTTLCacheClocked[entry[T]](MaxAge, now)}
}

// Get is key's value as load reads it, from the Cache when ctx is a read
// (Cacheable) whose request read the generation it was kept at. What it
// returns is shared: callers never change it.
func (c *Cache[T]) Get(ctx context.Context, key string, load func(context.Context) (T, error)) (T, error) {
	g, onRequest := ctx.Value(ctxKey{}).(read)
	db := database()
	if !onRequest || !IsCacheable(ctx) || db == nil {
		return load(ctx)
	}
	if !g.ok {
		c.entries.Purge()
		return load(ctx)
	}
	at := bumps.Load()
	if e, ok := c.entries.Get(key); ok && e.gen == g.n && e.bumps == at && e.db == db {
		return e.v, nil
	}
	v, err := load(ctx)
	if err != nil {
		return v, err
	}
	if c.entries.Len() >= maxKeys {
		c.entries.Purge()
	}
	c.entries.Set(key, entry[T]{gen: g.n, bumps: at, db: db, v: v})
	return v, nil
}

// database is the connection the rows are read through: an entry read
// through another (a test's own database) is never served.
func database() *sql.DB {
	if postgresInit.DBConn == nil {
		return nil
	}
	return postgresInit.DBConn.SqlDB
}
