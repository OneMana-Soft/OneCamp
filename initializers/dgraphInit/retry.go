package dgraphInit

import (
	"context"
	"errors"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// abortRetries is how many times a committed request is tried in all.
const abortRetries = 3

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
			case <-time.After(time.Duration(attempt*attempt) * 50 * time.Millisecond):
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
