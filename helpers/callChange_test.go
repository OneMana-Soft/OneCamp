package helpers

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A call's change id goes to the first change set the call makes, and only
// to that one; a ctx that named none gives none.
func TestACallsChangeIdIsTakenOnce(t *testing.T) {
	if id := TakeCallChange(context.Background()); id != uuid.Nil {
		t.Errorf("a ctx with no change id gave %s", id)
	}
	if _, ok := CallChange(context.Background()); ok {
		t.Error("a ctx with no change id names one")
	}
	id := uuid.New()
	ctx := WithCallChange(context.Background(), id)
	if first, second := TakeCallChange(ctx), TakeCallChange(ctx); first != id || second != uuid.Nil {
		t.Errorf("took %s, then %s; want %s, then none", first, second, id)
	}
	if named, ok := CallChange(ctx); !ok || named != id {
		t.Errorf("once taken, the call names %s (%v); want %s still", named, ok, id)
	}
}
