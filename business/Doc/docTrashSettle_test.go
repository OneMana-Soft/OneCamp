package businness

import (
	"context"
	"testing"
	"time"
)

// Trashing docs settles search before it answers (searchFollowsTrash). Its
// update ran under the request's ctx, with no bound of its own: so it had no
// time limit, and since the cascade honours its ctx, a closed tab would have
// dropped its conflict retries. It runs within settleTimeout of the call now,
// whatever becomes of the request.
func TestATrashSettleOutlivesItsRequestWithinItsBound(t *testing.T) {
	old := cascadeDeleteNow
	t.Cleanup(func() { cascadeDeleteNow = old })
	var errAtCall error
	var left time.Duration
	cascadeDeleteNow = func(ctx context.Context, _ map[string][]string, _ int64, _ []string, _ string) error {
		errAtCall = ctx.Err()
		if d, ok := ctx.Deadline(); ok {
			left = time.Until(d)
		}
		return nil
	}
	request, cancel := context.WithCancel(context.Background())
	cancel()

	if err := trashSettled(request, cascadeFields([]string{"d1"}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if errAtCall != nil {
		t.Errorf("the settle ran under the request's ctx, gone with it: %v", errAtCall)
	}
	if left <= 0 || left > settleTimeout {
		t.Errorf("the settle had %v left; want a bound of at most %v", left, settleTimeout)
	}
}
