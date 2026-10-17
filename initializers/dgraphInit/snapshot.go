package dgraphInit

import (
	"context"

	"github.com/dgraph-io/dgo/v230"
)

// InSnapshot runs fn in one read-only transaction: every query fn makes in
// txn sees the graph as the first one did. A read made of several queries (a
// list read a page at a time, a board read as places and then cards) needs
// that, or a write landing between two of them skips or repeats a task.
func InSnapshot(ctx context.Context, fn func(txn *dgo.Txn) error) error {
	txn := DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	return fn(txn)
}
