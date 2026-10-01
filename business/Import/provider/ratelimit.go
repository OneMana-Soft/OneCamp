// Per-provider rate limiting helpers.
//
// We use a simple token-bucket via golang.org/x/time/rate which is
// already pulled in by other parts of the codebase. Each provider
// constructs a *Limiter with parameters from its docs:
//
//	Asana   1500/min  → rate.NewLimiter(rate.Every(40ms), 50)
//	Jira    10/sec    → rate.NewLimiter(rate.Every(100ms), 10)
//	Trello  100/10s   → rate.NewLimiter(rate.Every(100ms), 10)
//	Notion  3/sec     → rate.NewLimiter(rate.Every(333ms), 3)
//	Todoist 450/15min → rate.NewLimiter(rate.Every(2s), 30)
package provider

import (
	"context"
	"time"
)

// Limiter is the minimal interface a provider's rate limiter must
// implement. We deliberately don't depend directly on x/time/rate here
// so a provider can supply a smarter sliding-window or quota-based
// limiter without contortion.
type Limiter interface {
	// Wait blocks until one token is available, or returns ctx.Err().
	Wait(ctx context.Context) error
}

// SleepLimiter is a simple "at-most-once-per-d" limiter used when
// x/time/rate isn't available. Concurrent callers serialise.
type SleepLimiter struct {
	d  time.Duration
	ch chan struct{}
}

// NewSleepLimiter ticks every d. Buffered to N to allow N-burst.
//
// The internal ticker goroutine runs for the process lifetime, which
// is intentional: each Provider is a process-wide singleton registered
// at init() time, so we only ever start one goroutine per provider.
// (Five providers = five goroutines = no leak surface.)
func NewSleepLimiter(d time.Duration, burst int) *SleepLimiter {
	if burst < 1 {
		burst = 1
	}
	l := &SleepLimiter{d: d, ch: make(chan struct{}, burst)}
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		// Pre-fill burst.
		for i := 0; i < burst; i++ {
			l.ch <- struct{}{}
		}
		for range t.C {
			select {
			case l.ch <- struct{}{}:
			default:
			}
		}
	}()
	return l
}

// Wait blocks until a token is available.
func (l *SleepLimiter) Wait(ctx context.Context) error {
	select {
	case <-l.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
