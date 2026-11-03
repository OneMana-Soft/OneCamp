package helpers

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"
)

// callChange is the change id a public API write names before it is served
// (business/PublicAPI invoke), so that its audit row and the change set its
// write makes carry the same id: an admin who reads the audit row finds every
// row the call touched. The first change set the call makes takes it; any
// later one gets an id of its own.
type callChange struct {
	id    uuid.UUID
	taken atomic.Bool
}

const callChangeKey ContextKey = "callChange"

// WithCallChange gives ctx's call the change id id.
func WithCallChange(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, callChangeKey, &callChange{id: id})
}

// CallChange is the change id ctx's call named, if it named one.
func CallChange(ctx context.Context) (uuid.UUID, bool) {
	c, _ := ctx.Value(callChangeKey).(*callChange)
	if c == nil {
		return uuid.Nil, false
	}
	return c.id, true
}

// TakeCallChange is the change id ctx's call named, for the first change set
// the call makes. Every later call, and a ctx that named none, gets uuid.Nil:
// that change set gets an id of its own.
func TakeCallChange(ctx context.Context) uuid.UUID {
	c, _ := ctx.Value(callChangeKey).(*callChange)
	if c == nil || !c.taken.CompareAndSwap(false, true) {
		return uuid.Nil
	}
	return c.id
}
