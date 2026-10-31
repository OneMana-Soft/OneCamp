package pgexec

import (
	"context"

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
		if _, err := Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key); err != nil {
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
