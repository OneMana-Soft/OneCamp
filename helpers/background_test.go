package helpers

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// registerForTest registers work under name for one test only.
func registerForTest(t *testing.T, name string, settle func(time.Duration) bool) {
	t.Helper()
	RegisterBackground(name, settle)
	t.Cleanup(func() {
		backgroundMu.Lock()
		delete(backgroundList, name)
		backgroundMu.Unlock()
	})
}

func TestInflightSettlesWhenTheWorkIsDone(t *testing.T) {
	var f Inflight
	if !f.Settle(0) {
		t.Fatal("nothing started, and Settle says something is in flight")
	}
	f.Add(2)
	if f.Settle(10 * time.Millisecond) {
		t.Fatal("two in flight, and Settle says none")
	}
	done := make(chan bool)
	go func() { done <- f.Settle(time.Minute) }()
	f.Add(-1)
	// Work that starts while Settle waits is waited for too.
	f.Add(1)
	f.Add(-1)
	select {
	case <-done:
		t.Fatal("Settle returned with one still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	f.Add(-1)
	if !<-done {
		t.Fatal("Settle didn't see the work finish")
	}
	// Again from idle: the count can rise and fall more than once.
	f.Add(1)
	f.Add(-1)
	if !f.Settle(0) {
		t.Fatal("idle again, and Settle says otherwise")
	}
}

func TestInflightRefusesMoreFinishedThanStarted(t *testing.T) {
	var f Inflight
	defer func() {
		if recover() == nil {
			t.Fatal("finishing work that never started went unnoticed")
		}
	}()
	f.Add(-1)
}

// Settling one kind of work can start another (an event's sink writes an
// audit row): SettleBackground goes round until everything is idle at once.
func TestSettleBackgroundWaitsForWorkThatStartsMoreWork(t *testing.T) {
	var a, b Inflight
	registerForTest(t, "test: a", a.Settle)
	registerForTest(t, "test: b", b.Settle)
	// b starts a as it finishes, so a pass that found a idle must look again.
	b.Add(1)
	releaseA := make(chan struct{})
	go func() {
		a.Add(1)
		b.Add(-1)
		<-releaseA
		a.Add(-1)
	}()
	settled := make(chan []string, 1)
	go func() { settled <- SettleBackground(time.Minute) }()
	select {
	case busy := <-settled:
		t.Fatalf("returned (busy %v) while the work b started was running", busy)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseA)
	if busy := <-settled; len(busy) != 0 {
		t.Fatalf("still busy: %v", busy)
	}
	if !a.Settle(0) || !b.Settle(0) {
		t.Fatal("SettleBackground returned with work in flight")
	}
}

// Work that never finishes is named once the wait is up, and the wait ends:
// a SettleBackground that ignored its bound would still be waiting a minute
// later, which no machine's load explains.
func TestSettleBackgroundNamesWhatIsStillRunning(t *testing.T) {
	var stuck Inflight
	registerForTest(t, "test: stuck", stuck.Settle)
	stuck.Add(1)
	defer stuck.Add(-1)
	done := make(chan []string, 1)
	go func() { done <- SettleBackground(30 * time.Millisecond) }()
	select {
	case busy := <-done:
		if !slices.Contains(busy, "test: stuck") {
			t.Fatalf("busy %v, want it to name the stuck work", busy)
		}
	case <-time.After(time.Minute):
		t.Fatal("SettleBackground is still waiting a minute past its 30 ms bound")
	}
}

// Goroutines started with GoSafe, GoSafeNamed and GoSafeSend are counted until
// they finish, panicking or not.
func TestGoSafeGoroutinesAreWaitedFor(t *testing.T) {
	release := make(chan struct{})
	var ran atomic.Int32
	GoSafe(func() { <-release; ran.Add(1) })
	GoSafeNamed("test", func() { <-release; ran.Add(1); panic("after its work") })
	ch := make(chan int, 1)
	GoSafeSend("test", ch, func() int { <-release; ran.Add(1); return 1 })
	if goSafeWork.Settle(20 * time.Millisecond) {
		t.Fatal("three goroutines blocked, and they count as done")
	}
	close(release)
	if busy := SettleBackground(time.Minute); len(busy) != 0 {
		t.Fatalf("still busy: %v", busy)
	}
	if ran.Load() != 3 {
		t.Fatalf("%d of the 3 had finished when they were said to be", ran.Load())
	}
}

// A loop lasts as long as its context and is not work in flight: settling
// doesn't wait for it, and a panic in it is recovered like GoSafe's.
func TestGoSafeLoopIsNotWaitedFor(t *testing.T) {
	stop, exited := make(chan struct{}), make(chan struct{})
	GoSafeLoop("test loop", func() {
		defer close(exited)
		<-stop
		panic("as it stops")
	})
	if !goSafeWork.Settle(0) {
		t.Fatal("a loop is counted as work in flight")
	}
	close(stop)
	<-exited
}
