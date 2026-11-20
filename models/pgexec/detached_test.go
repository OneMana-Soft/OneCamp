package pgexec

import (
	"context"
	"database/sql"
	"testing"
)

// Detached is a request's ctx for work that outlives the request: its values
// stay, its cancel and any transaction it holds don't (V-1119 F1). By the time
// the work runs the request has committed or rolled that transaction back, so
// a statement made with Detached's ctx runs on the pool, as one made with
// context.Background() did (CR-378), and AfterCommit runs its fn at once.
func TestDetachedKeepsTheValuesButNotTheCancelOrTheTransaction(t *testing.T) {
	type traceKey struct{}
	tx := &sql.Tx{}
	req := context.WithValue(WithTx(context.WithValue(context.Background(), traceKey{}, "trace"), tx), afterKey{}, &afterCommit{})
	req, end := context.WithCancel(req)
	end()

	ctx := Detached(req)
	if ctx.Value(traceKey{}) != "trace" {
		t.Error("Detached lost the request's values")
	}
	if ctx.Done() != nil || ctx.Err() != nil {
		t.Errorf("Detached ends with the request (%v)", ctx.Err())
	}
	if inTx(ctx) || ranWithoutWorkers(ctx) {
		t.Error("Detached carries the request's transaction")
	}
	if q, _ := db(ctx); q == Querier(tx) {
		t.Error("a statement made with Detached's ctx runs in the request's transaction")
	}
	ran := false
	AfterCommit(ctx, func(context.Context) { ran = true })
	if !ran {
		t.Error("AfterCommit with Detached's ctx waits for the request's commit")
	}
}
