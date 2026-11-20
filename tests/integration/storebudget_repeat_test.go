//go:build integration
// +build integration

package integration_test

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
)

// TestStoreBudgetRepeatabilityHoldsSyncOnly is the store budgets' run-to-run
// check on two made-up runs of one row, so it starts no server. b320's
// integration run at load 17-29 counted dm createChat with the same sync
// stores in every run and async 7 in one, 5 in another: the async counts are
// by time window, so under load a route's background work lands in or out of
// its window, and they are reported, not held.
func TestStoreBudgetRepeatabilityHoldsSyncOnly(t *testing.T) {
	sync := integration.Counts{2, 2, 0, 2, 0} // pg dg os redis mqtt
	calls := []string{"pg SELECT chat", "pg INSERT chat", "dg /api.Dgraph/Query", "dg /api.Dgraph/Mutate", "redis get", "redis set"}
	asyncCalls := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "redis publish"
		}
		return out
	}
	first := integration.Budget{Route: "dm createChat", Sync: sync, Async: 7}
	a := integration.Window{Sync: sync, Async: integration.Counts{0, 0, 0, 7, 0}, SyncCalls: calls, AsyncCalls: asyncCalls(7)}

	t.Run("two runs that differ only in async agree", func(t *testing.T) {
		again := integration.Budget{Route: "dm createChat", Sync: sync, Async: 5}
		b := integration.Window{Sync: sync, Async: integration.Counts{0, 0, 0, 5, 0}, SyncCalls: calls, AsyncCalls: asyncCalls(5)}
		if bad := integration.Unrepeatable(2, first, again, a, b); bad != "" {
			t.Errorf("async 7 then 5 with the same sync stores failed the row: %s", bad)
		}
	})

	t.Run("two runs that differ in one sync store fail, naming it", func(t *testing.T) {
		more := sync
		more[integration.StorePostgres]++
		again := integration.Budget{Route: "dm createChat", Sync: more, Async: 7}
		b := integration.Window{Sync: more, Async: a.Async, SyncCalls: append(append([]string{}, calls...), "pg SELECT 1"), AsyncCalls: asyncCalls(7)}
		bad := integration.Unrepeatable(2, first, again, a, b)
		if !strings.Contains(bad, "dm createChat is not repeatable") || !strings.Contains(bad, "sync +1 pg SELECT 1") {
			t.Errorf("one more sync pg call than run 1 said %q; want the row not repeatable, naming sync +1 pg SELECT 1", bad)
		}
	})
}

// TestStoreBudgetIdleWindowHoldsSyncOnly is the check of the window of a call
// that made none, which TestStoreBudgets opens before its rows: under load
// the setup's background work lands in it as a route's lands in the next
// row's, so its async is printed, and only a sync call fails.
func TestStoreBudgetIdleWindowHoldsSyncOnly(t *testing.T) {
	t.Run("async in an idle window is reported, not enforced", func(t *testing.T) {
		bad, note := integration.IdleWindow(integration.Window{Async: integration.Counts{0, 0, 0, 4, 0}, AsyncCalls: []string{"redis get", "redis get", "redis set", "redis publish"}})
		if bad != "" {
			t.Errorf("4 async calls in an idle window failed the test: %s", bad)
		}
		if !strings.Contains(note, "idle window counted 4 async (reported, not enforced; V-1121)") {
			t.Errorf("4 async calls in an idle window said %q; want them reported", note)
		}
	})

	t.Run("a sync call in an idle window fails", func(t *testing.T) {
		bad, _ := integration.IdleWindow(integration.Window{Sync: integration.Counts{1, 0, 0, 0, 0}, SyncCalls: []string{"pg SELECT 1"}})
		if !strings.Contains(bad, "an idle window counted sync pg=1") {
			t.Errorf("a sync pg call in an idle window said %q; want it to fail, naming pg=1", bad)
		}
	})
}
