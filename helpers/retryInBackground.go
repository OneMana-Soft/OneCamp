package helpers

import (
	"context"
	"time"
)

// Retries is how patiently a background job tries: First is the wait before
// its first try, doubled after each failure while it is under Cap (no cap
// when Cap is 0), for at most Tries tries in one process.
type Retries struct {
	First time.Duration
	Cap   time.Duration
	Tries int
}

// retryAfter is time.After, a seam so a test can count the waits instead of
// sitting through them.
var retryAfter = time.After

// RetryInBackground runs try on a goroutine of its own until a try succeeds,
// ctx ends, or the tries run out. It is for the jobs a server starts with it:
// a repair or backfill run once per install, a pass run at every start. They
// wait for stores a starting server may not have yet, and never delay or fail
// the start. It waits r.First before the first try and twice as long after
// each failure. What a try did, and why it failed, is the try's to log. The
// goroutine is GoSafeLoop's: recovered, and not counted as work in flight,
// since it may go on retrying for hours.
func RetryInBackground(ctx context.Context, name string, r Retries, try func(context.Context) error) {
	GoSafeLoop(name, func() { retry(ctx, r, try) })
}

// retry is RetryInBackground's loop, on the caller's goroutine.
func retry(ctx context.Context, r Retries, try func(context.Context) error) {
	wait := r.First
	for i := 0; i < r.Tries; i++ {
		select {
		case <-ctx.Done():
			return
		case <-retryAfter(wait):
		}
		if try(ctx) == nil {
			return
		}
		if r.Cap <= 0 || wait < r.Cap {
			wait *= 2
		}
	}
}
