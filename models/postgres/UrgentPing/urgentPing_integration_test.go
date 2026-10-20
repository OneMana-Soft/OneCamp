//go:build integration

package models_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	pings "github.com/akashc777/OneCamp/models/postgres/UrgentPing"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// "Notify anyway" allows one urgent ping a day per pair. It counted the pair's
// pings, then wrote one, so taps at once could both count none and both go
// through (2 of 12 here, before the pair's lock): two urgent pushes through
// a teammate's pause where the rule says one. The reverse pair is its own.
func TestOneUrgentPingADayHoldsWhenTapsArriveAtOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	me, them := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{me, them} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, []string{"me@example.test", "them@example.test"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().Add(-24 * time.Hour)
	const taps = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	start := make(chan struct{})
	for i := 0; i < taps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := pings.Claim(ctx, me, them, since, 1)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	var rows int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM urgent_pings WHERE sender_id = $1 AND recipient_id = $2`, me, them).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if claimed != 1 || rows != 1 {
		t.Fatalf("%d taps at once: %d claimed, %d recorded; want 1 and 1", taps, claimed, rows)
	}
	// The other way round is another pair.
	if ok, err := pings.Claim(ctx, them, me, since, 1); err != nil || !ok {
		t.Errorf("the reverse pair: %v %v", ok, err)
	}
}
