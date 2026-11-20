//go:build integration
// +build integration

package integration_test

// A one-off job's record moves only from the value its writer saw
// (configModel.Swap, as the transcripts backfill records itself done only
// from the state its pass started in): against Postgres 12, a swap from the
// value the row holds writes it once, however many replicas race for it; a
// swap from a value since replaced writes nothing and says so; and a swap
// from "" claims a key nobody holds, once.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAConfigSwapWritesOnlyFromTheValueItSaw -v

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAConfigSwapWritesOnlyFromTheValueItSaw(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the pool at the test database: %v", err)
	}
	key, bystander := "tests.swap."+uuid.NewString(), "tests.swap."+uuid.NewString()
	type record struct {
		value string
		at    time.Time
	}
	read := func(key string) record {
		t.Helper()
		cfg, err := configModel.GetConfigByKey(key)
		if err != nil || cfg == nil {
			t.Fatalf("no record under %q: %v", key, err)
		}
		return record{cfg.Value, cfg.UpdatedAt}
	}
	swap := func(from, to string, want bool) {
		t.Helper()
		if wrote, err := configModel.Swap(ctx, key, from, to); err != nil || wrote != want {
			t.Fatalf("swapping %q from %q to %q answered %v (%v), want %v", key, from, to, wrote, err, want)
		}
	}

	// From "" with no row: the first claims the key, a second claims nothing.
	swap("", "pending", true)
	swap("", "claimed again", false)
	if got := read(key); got.value != "pending" {
		t.Fatalf("a second claim left %q, want the first's %q", got.value, "pending")
	}
	if wrote, err := configModel.Claim(ctx, bystander, "untouched"); err != nil || !wrote {
		t.Fatalf("claiming the bystander key: %v (%v)", wrote, err)
	}

	// From the value the row holds, racing replicas: exactly one writes.
	const racers = 8
	won := make(chan string, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func(to string) {
			defer wg.Done()
			wrote, err := configModel.Swap(ctx, key, "pending", to)
			if err != nil {
				t.Errorf("racer %s: %v", to, err)
			}
			if wrote {
				won <- to
			}
		}(fmt.Sprintf("done %d", i))
	}
	wg.Wait()
	close(won)
	var winners []string
	for to := range won {
		winners = append(winners, to)
	}
	if len(winners) != 1 {
		t.Fatalf("%d of %d swaps from %q wrote (%q), want exactly one", len(winners), racers, "pending", winners)
	}
	after := read(key)
	if after.value != winners[0] {
		t.Fatalf("the row holds %q, want the one writer's %q", after.value, winners[0])
	}

	// From a value since replaced: nothing moves, value nor time, and it says so.
	swap("pending", "stale", false)
	if got := read(key); got.value != after.value || !got.at.Equal(after.at) {
		t.Errorf("a stale swap moved the row to %+v from %+v", got, after)
	}
	// And a swap never reaches another key's row.
	if got := read(bystander); got.value != "untouched" {
		t.Errorf("the bystander key holds %q, want %q", got.value, "untouched")
	}
}
