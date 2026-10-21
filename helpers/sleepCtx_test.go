package helpers

import (
	"context"
	"testing"
	"time"
)

// A pause ends when its ctx does: retries and start-up pauses that slept
// through a shutdown or a cancelled request were time.Sleep.
func TestSleepCtxEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if SleepCtx(ctx, time.Minute) {
		t.Fatal("a cancelled ctx slept the whole pause")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a cancelled ctx still paused %s", time.Since(start))
	}
	if !SleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("a live ctx should sleep the whole pause")
	}
	if !SleepCtx(context.Background(), 0) || SleepCtx(ctx, 0) {
		t.Fatal("a zero pause answers whether ctx is live")
	}
}
