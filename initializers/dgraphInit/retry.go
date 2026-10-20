package dgraphInit

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// abortRetries is how many times a committed request is tried in all. Three
// weren't enough for a burst: of 8 posts written into one channel at once,
// 4 landed (tests/integration/messages_at_once_test.go).
const abortRetries = 8

// abortWait is how long the attempt'th try (from 1) waits: a backoff growing
// with the square of the attempt, up to 3.5 s in all, half of it at random so
// writers aborted together don't collide again on the same beat.
func abortWait(attempt int) time.Duration {
	step := time.Duration(attempt*attempt) * 25 * time.Millisecond
	return step/2 + time.Duration(rand.Int64N(int64(step/2)+1))
}

// DoCommitNow runs a CommitNow request (an upsert, usually) in its own
// transaction, and runs it again in a fresh one when Dgraph aborts it.
//
// Dgraph aborts a transaction that conflicts with a concurrent one and says
// "Please retry". Two writes to the same user at once (sign-in and the agent
// bot being ensured, say) is ordinary, and before this the loser simply
// failed: 29 user writes in a month, agent setup among them. A CommitNow
// request is all-or-nothing, so running it again is safe.
func DoCommitNow(ctx context.Context, req *api.Request) (*api.Response, error) {
	req.CommitNow = true
	var lastErr error
	for attempt := 0; attempt < abortRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(abortWait(attempt)):
			}
		}
		txn := DgraphClient.NewTxn()
		res, err := txn.Do(ctx, req)
		_ = txn.Discard(ctx)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !errors.Is(err, dgo.ErrAborted) {
			return nil, err
		}
	}
	return nil, lastErr
}

// InTxn runs fn in one read-write transaction and commits what it wrote, so
// what fn read and decided on is still true when its write lands. When Dgraph
// aborts the commit because a concurrent transaction wrote the same thing,
// fn runs again in a fresh transaction and sees that write. An error from fn
// is returned as it is, with nothing written.
func InTxn(ctx context.Context, fn func(txn *dgo.Txn) error) error {
	var lastErr error
	for attempt := 0; attempt < abortRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(abortWait(attempt)):
			}
		}
		txn := DgraphClient.NewTxn()
		err := fn(txn)
		if err == nil {
			err = txn.Commit(ctx)
		}
		_ = txn.Discard(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, dgo.ErrAborted) {
			return err
		}
	}
	return lastErr
}
