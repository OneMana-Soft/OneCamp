package business

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// A burst of deliveries starts at most as many goroutines as the send pool
// has places, at once and without holding up the caller; the rest stay queued
// for the drain, whose rows are in Postgres. F4 fixed the old webhook sender
// the same way (its pool before the goroutine), and P-API's Hooks had the
// goroutine first again.
func TestABurstStartsNoMoreSendsThanThePoolHolds(t *testing.T) {
	f := setup(t)
	prev := sending
	t.Cleanup(func() { sending = prev })
	sending = make(chan struct{}, 2)

	// The endpoint holds every request until the test lets it go, so the
	// sends in flight stay in flight while the test counts them.
	var inFlight, most atomic.Int32
	release := make(chan struct{})
	hold := func() {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-release
		inFlight.Add(-1)
	}
	f.onRequest.Store(&hold)

	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, f.queue(t, `{"n":1}`).ID)
	}
	done := make(chan struct{})
	go func() { SendNow(ids); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a full pool held up the caller")
	}
	for deadline := time.Now().Add(5 * time.Second); inFlight.Load() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d sends started", inFlight.Load())
		}
	}
	time.Sleep(200 * time.Millisecond)
	if m := most.Load(); m > 2 {
		t.Fatalf("%d sends ran at once with 2 places", m)
	}
	close(release)
	// Every send this test started is finished before it reads the store or
	// its cleanup puts the package's seams back (helpers' count of work in
	// flight, behind a mutex, so the reads below happen after the sends').
	if stuck := helpers.SettleBackground(10 * time.Second); len(stuck) > 0 {
		t.Fatalf("sends still running: %v", stuck)
	}
	if len(sending) != 0 {
		t.Fatal("a send kept its place")
	}
	// The three that found no place are still queued, for the drain.
	if pending := f.mem.untried(); pending != 3 {
		t.Fatalf("%d deliveries are left for the drain, want 3", pending)
	}
}
