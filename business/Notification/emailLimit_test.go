package notification

import (
	"context"
	"testing"

	emailService "github.com/akashc777/OneCamp/services/Email"
)

func TestTheDaysEmailIsSpentOnlyUnderACapWithNoneLeft(t *testing.T) {
	prev := leftToday
	t.Cleanup(func() { leftToday = prev })
	for _, c := range []struct {
		left          int
		capped, spent bool
	}{{0, false, false}, {3, true, false}, {0, true, true}} {
		leftToday = func(keep int) (int, bool) {
			if keep != emailService.InvitationReserve {
				t.Errorf("kept %d, not the resets' reserve", keep)
			}
			return c.left, c.capped
		}
		if got := EmailLimitReached(context.Background()); got != c.spent {
			t.Errorf("%d left, capped %v: spent %v", c.left, c.capped, got)
		}
	}
}
