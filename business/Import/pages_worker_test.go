package business

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A tree's chunk renews its claim on every beat, with how many pages have
// been read by then, until it is done: reading and making a large tree takes
// longer than the reaper waits, which would hand it to a second worker.
func TestATreesClaimIsRenewedUntilItIsDone(t *testing.T) {
	savedEvery, savedRenew := heartbeatEvery, renewClaim
	t.Cleanup(func() { heartbeatEvery, renewClaim = savedEvery, savedRenew })
	heartbeatEvery = 5 * time.Millisecond
	var mu sync.Mutex
	var renewed []int
	renewClaim = func(_ context.Context, _ uuid.UUID, read int, _ *string) error {
		mu.Lock()
		defer mu.Unlock()
		renewed = append(renewed, read)
		return nil
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(renewed)
	}

	var read atomic.Int64
	read.Store(7)
	stop := keepClaimed(context.Background(), uuid.New(), func() int { return int(read.Load()) })
	for deadline := time.Now().Add(5 * time.Second); count() < 3 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	stop()
	n := count()
	if n < 3 || renewed[0] != 7 {
		t.Fatalf("renewed %v", renewed)
	}
	time.Sleep(30 * time.Millisecond)
	if count() != n {
		t.Fatal("the claim was renewed after the tree was done")
	}
}

// A beat that panics ends the beat, never the server, and the tree's work
// still stops it: it ran on a bare goroutine, where a panic took the whole
// process down with every other import.
func TestATreesClaimThatPanicsEndsOnlyItsBeat(t *testing.T) {
	savedEvery, savedRenew := heartbeatEvery, renewClaim
	t.Cleanup(func() { heartbeatEvery, renewClaim = savedEvery, savedRenew })
	heartbeatEvery = time.Millisecond
	var beats atomic.Int32
	renewClaim = func(context.Context, uuid.UUID, int, *string) error {
		beats.Add(1)
		panic("the claim's table is gone")
	}
	stop := keepClaimed(context.Background(), uuid.New(), func() int { return 0 })
	for deadline := time.Now().Add(5 * time.Second); beats.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if beats.Load() == 0 {
		t.Fatal("the claim was never renewed")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never returned after the beat panicked")
	}
}
