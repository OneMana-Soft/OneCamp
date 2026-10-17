//go:build integration
// +build integration

package integration

// The one drain: tests wait for the work they handed off through here.

import (
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// settleWait bounds the wait. Every kind of work it waits for has a deadline
// of its own well inside it (an event's sink has two minutes), so work still
// running past it is stuck, not slow.
const settleWait = 3 * time.Minute

// Settle waits until the work the test handed off has finished: everything
// registered with helpers.RegisterBackground, which is the rows queued for the
// audit log, events' sinks, and the goroutines started with GoSafe (docs'
// search entries and save hooks, AI entries among them). Call it before
// reading what that work writes. The test fails, naming the work, if it is
// still running after settleWait.
func Settle(t *testing.T) {
	t.Helper()
	if busy := helpers.SettleBackground(settleWait); len(busy) > 0 {
		t.Fatalf("still running after %s: %v", settleWait, busy)
	}
}

// settleBeforeTeardown has the test, as it ends, wait for the work it handed
// off before the thing just set up is taken down. Every Setup function calls
// it right after registering its own teardown: cleanups run last first, so
// the wait comes before that teardown, and the first wait to run does the
// waiting. Work still running after settleWait is reported by name; the
// teardowns still run.
func settleBeforeTeardown(t *testing.T) {
	t.Cleanup(func() {
		if busy := helpers.SettleBackground(settleWait); len(busy) > 0 {
			t.Errorf("work the test handed off was still running %s after it ended: %v", settleWait, busy)
		}
	})
}
