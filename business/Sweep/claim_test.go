package business

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claims is a shared claim store, as Redis is for two instances: the first to
// ask for a slot wins it.
type claims struct {
	mu    sync.Mutex
	won   map[int64]string
	asked map[int64]int
	wins  int
}

func (c *claims) for_(who string) claimer {
	return func(_ context.Context, _ string, slot int64, _ time.Duration) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.asked[slot]++
		if _, taken := c.won[slot]; taken {
			return false
		}
		c.won[slot] = who
		c.wins++
		return true
	}
}

// Two instances tick the same job; between them it runs once a slot, never
// twice, and not at all on a tick whose slot the other already ran.
func TestASweepClaimedByOneOfTwoInstancesRunsOnce(t *testing.T) {
	store := &claims{won: map[int64]string{}, asked: map[int64]int{}}
	var runs atomic.Int32
	job := func() Job {
		return Job{Name: "doc.trash.purge", Every: 20 * time.Millisecond, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}}
	}
	a := &Registry{claim: store.for_("a")}
	b := &Registry{claim: store.for_("b")}
	a.Register(job())
	b.Register(job())

	ctx, cancel := context.WithCancel(context.Background())
	a.StartAll(ctx)
	time.Sleep(7 * time.Millisecond) // out of phase, inside the same slots
	b.StartAll(ctx)
	waitFor(t, func() bool { return runs.Load() >= 6 })
	cancel()
	time.Sleep(30 * time.Millisecond)

	store.mu.Lock()
	defer store.mu.Unlock()
	if int(runs.Load()) != store.wins {
		t.Errorf("the job ran %d times for %d claims won", runs.Load(), store.wins)
	}
	// Both instances ticked in the same slots, and each slot ran once: the
	// second tick in a slot found it claimed.
	contended := 0
	for _, n := range store.asked {
		if n >= 2 {
			contended++
		}
	}
	if contended == 0 {
		t.Error("the two instances never ticked in the same slot, so nothing was tested")
	}
}

// One instance's ticks are an interval apart, so no two fall in one slot.
func TestATicksSlotIsTheIntervalItFallsIn(t *testing.T) {
	every := time.Hour
	start := time.Date(2026, 10, 12, 10, 20, 0, 0, time.UTC)
	seen := map[int64]bool{}
	for k := 0; k < 48; k++ {
		s := slotOf(start.Add(time.Duration(k)*every), every)
		if seen[s] {
			t.Fatalf("tick %d shares slot %d with an earlier one", k, s)
		}
		seen[s] = true
	}
	// Two instances out of phase within one hour share the slot.
	if slotOf(start, every) != slotOf(start.Add(30*time.Minute), every) {
		t.Error("ticks 10:20 and 10:50 should contend for the 10:00 slot")
	}
}

// Without Redis, every tick claims: a single instance runs every pass.
func TestWithoutRedisEveryTickClaims(t *testing.T) {
	if !claimInRedis(context.Background(), "a", 1, time.Minute) {
		t.Error("with no Redis configured, a tick did not claim its slot")
	}
}

// A job that claims its own work runs on every instance, every pass: the
// slot's claim is never asked.
func TestAJobThatClaimsItsOwnWorkRunsEverywhere(t *testing.T) {
	var asked atomic.Int32
	never := func(context.Context, string, int64, time.Duration) bool { asked.Add(1); return false }
	var runs atomic.Int32
	a, b := &Registry{claim: never}, &Registry{claim: never}
	for _, r := range []*Registry{a, b} {
		r.Register(Job{Name: "content-io.run", Every: 10 * time.Millisecond, ClaimsItsOwnWork: true, Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.StartAll(ctx)
	b.StartAll(ctx)
	waitFor(t, func() bool { return runs.Load() >= 6 })
	cancel()
	if n := asked.Load(); n != 0 {
		t.Errorf("the claim was asked %d times for a job that claims its own work", n)
	}
}
