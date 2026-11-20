package pgexec

import (
	"context"
	"errors"
	"testing"
)

// A transaction lock taken outside a transaction ends with its own statement,
// so LockXact refuses there before it reaches the database: no ctx without a
// transaction gets a lock that looks held and isn't.
func TestLockXactRefusesOutsideATransaction(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no transaction":           context.Background(),
		"a nil transaction handed": WithTx(context.Background(), nil),
		"work detached from one":   Detached(context.Background()),
	} {
		if err := LockXact(ctx, "h8.lockxact"); !errors.Is(err, ErrNotInTx) {
			t.Errorf("%s: LockXact = %v, want ErrNotInTx", name, err)
		}
	}
}
