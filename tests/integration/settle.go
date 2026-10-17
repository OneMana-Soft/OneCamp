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
// registered with helpers.RegisterBackground, such as the rows queued for the
// audit log. Call it before reading what that work writes. The test fails,
// naming the work, if it is still running after settleWait.
func Settle(t *testing.T) {
	t.Helper()
	if busy := helpers.SettleBackground(settleWait); len(busy) > 0 {
		t.Fatalf("still running after %s: %v", settleWait, busy)
	}
}
