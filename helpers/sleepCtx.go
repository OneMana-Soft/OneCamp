package helpers

import (
	"context"
	"time"
)

// SleepCtx pauses for d, or until ctx ends, whichever comes first. True when
// it slept the whole pause; false when ctx ended first, so a retry loop or a
// worker's start-up pause returns rather than sleeping through a shutdown or a
// cancelled request. A pause of zero or less returns straight away, true
// unless ctx has already ended.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
