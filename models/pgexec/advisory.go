package pgexec

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// TryAdvisoryLock takes the session advisory lock key on a connection of its
// own, if no other session holds it: held false (and nothing to release)
// when another does. release gives it back before the connection returns to
// the pool, even once ctx has ended: a pooled connection still holding it
// would keep every replica from taking it until the pool closed it.
func TryAdvisoryLock(ctx context.Context, key int64) (release func() error, held bool, err error) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return nil, false, ErrNoDatabase
	}
	c, cancel := statementCtx(ctx)
	defer cancel()
	conn, err := postgresInit.DBConn.SqlDB.Conn(c)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRowContext(c, `SELECT pg_try_advisory_lock($1)`, key).Scan(&held); err != nil || !held {
		_ = conn.Close()
		return nil, false, err
	}
	return func() error {
		defer conn.Close()
		rc, cancel := statementCtx(context.WithoutCancel(ctx))
		defer cancel()
		_, err := conn.ExecContext(rc, `SELECT pg_advisory_unlock($1)`, key)
		return err
	}, true, nil
}

// Capped runs add, in one transaction (InTx), when count says fewer than
// limit are there, and says whether it ran. The count and add hold the
// transaction lock named key: counted apart from the write, two writes at
// once could each see room for the last one. A call's polls and questions
// are capped this way.
func Capped(ctx context.Context, key string, limit int, count func(context.Context) (int, error), add func(context.Context) error) (bool, error) {
	added := false
	err := InTx(ctx, func(ctx context.Context) error {
		if err := LockXact(ctx, key); err != nil {
			return err
		}
		n, err := count(ctx)
		if err != nil || n >= limit {
			return err
		}
		if err := add(ctx); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added && err == nil, err
}

// ErrNotInTx is what LockXact returns outside a transaction: a transaction
// lock taken there ends with its own statement and guards nothing.
var ErrNotInTx = errors.New("pgexec: a transaction lock needs a transaction (take it inside InTx)")

// WideKey names a transaction lock by text hashed to 64 bits
// (hashtextextended(key, 0)), the way the tables' locks are named
// (models/postgres/DataTable): the same name as a string would be a different
// lock.
type WideKey string

// LockXact takes the transaction advisory lock named key in the transaction
// ctx carries (InTx, InTxFor or WithTx); the transaction's end lets it go.
// Outside one it returns ErrNotInTx without a round trip. A string key is
// hashed with hashtext, a WideKey with hashtextextended, and an int64 is the
// key itself: each the value its lock had before LockXact, since a key that
// changed would let a process of the last release and one of this release,
// running side by side while one replaces the other, each hold "the lock" at
// once. Every transaction lock goes through here
// (helpers/advisoryLockGuard_test.go).
func LockXact[K string | WideKey | int64](ctx context.Context, key K) error {
	if !inTx(ctx) {
		return ErrNotInTx
	}
	q, arg := `SELECT pg_advisory_xact_lock(hashtext($1))`, any(key)
	switch k := any(key).(type) {
	case WideKey:
		q, arg = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(k)
	case int64:
		q = `SELECT pg_advisory_xact_lock($1)`
	}
	_, err := Exec(ctx, q, arg)
	return err
}
