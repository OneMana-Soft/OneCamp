package helpers

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// A window gathers each key's items from its first Add until its time is up,
// then hands them over together, once; what comes after opens the next.
func TestWindowsGatherEachKeyUntilItsTimeIsUp(t *testing.T) {
	var (
		fires []func()
		waits []time.Duration
		got   []string
	)
	w := &Windows[int]{
		Wait:  5 * time.Second,
		Flush: func(key string, items []int) { got = append(got, fmt.Sprint(key, items)) },
		Start: func(d time.Duration, f func()) { waits = append(waits, d); fires = append(fires, f) },
	}
	w.Add("a", 1)
	w.Add("b", 2)
	w.Add("a", 3, 4)
	w.Add("c")
	if len(fires) != 2 || waits[0] != 5*time.Second || waits[1] != 5*time.Second {
		t.Fatalf("one timer of Wait per key's window, none for nothing: %d timers %v", len(fires), waits)
	}
	if len(got) != 0 {
		t.Fatalf("handed over before its time: %v", got)
	}
	fires[0]()
	if want := []string{"a[1 3 4]"}; !slices.Equal(got, want) {
		t.Fatalf("a's window: %v, want %v", got, want)
	}
	// a's window is over: the next Add opens another, and the old timer
	// firing again hands over nothing, early or twice.
	w.Add("a", 5)
	fires[0]()
	if len(fires) != 3 || len(got) != 1 {
		t.Fatalf("after a's window: %d timers, handed over %v", len(fires), got)
	}
	fires[1]()
	fires[2]()
	if want := []string{"a[1 3 4]", "b[2]", "a[5]"}; !slices.Equal(got, want) {
		t.Fatalf("handed over %v, want %v", got, want)
	}
}

// Many adders at once lose nothing, and the real timer hands each window over
// once its Wait has passed.
func TestWindowsTimeUpOnTheirOwn(t *testing.T) {
	done := make(chan []int, 64)
	w := &Windows[int]{Wait: 50 * time.Millisecond, Flush: func(_ string, items []int) { done <- items }}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() { defer wg.Done(); w.Add("k", i) }()
	}
	wg.Wait()
	select {
	case items := <-done:
		if len(items) != 40 {
			t.Fatalf("handed over %d of 40", len(items))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the window never closed")
	}
	select {
	case items := <-done:
		t.Fatalf("handed over twice: %v", items)
	case <-time.After(150 * time.Millisecond):
	}
}

// An origin names one action; the first one marked stays, and a context
// never marked has none.
func TestOriginNamesOneAction(t *testing.T) {
	if _, ok := OriginOf(context.Background()); ok {
		t.Fatal("an unmarked context has an origin")
	}
	ctx := WithOrigin(context.Background(), "bulk-1")
	if id, ok := OriginOf(ctx); !ok || id != "bulk-1" {
		t.Fatalf("origin %q %v, want bulk-1", id, ok)
	}
	if id, _ := OriginOf(WithOrigin(ctx, "bulk-2")); id != "bulk-1" {
		t.Fatalf("an inner action took over the outer one's origin: %q", id)
	}
	if _, ok := OriginOf(WithOrigin(context.Background(), "")); ok {
		t.Fatal("an empty id is an origin")
	}
}

// An open window is work in flight (background.go): a settle waits until it
// has been handed over, so a graceful shutdown pushes what it gathered and
// one test's window can't land in the next; a Flush that panics still
// releases it (CR-271 F1).
func TestAnOpenWindowIsWaitedFor(t *testing.T) {
	flushed := make(chan struct{})
	w := &Windows[int]{Wait: 100 * time.Millisecond, Flush: func(string, []int) {
		time.Sleep(20 * time.Millisecond)
		close(flushed)
	}}
	w.Add("k", 1)
	if busy := SettleBackground(5 * time.Second); len(busy) != 0 {
		t.Fatalf("still busy after the settle: %v", busy)
	}
	select {
	case <-flushed:
	default:
		t.Fatal("the settle returned before the open window was handed over")
	}

	var fire func()
	p := &Windows[int]{Wait: time.Hour, Flush: func(string, []int) { panic("a Flush that fails") },
		Start: func(_ time.Duration, f func()) { fire = f }}
	p.Add("k", 1)
	func() {
		defer func() { _ = recover() }()
		fire()
	}()
	if busy := SettleBackground(time.Second); len(busy) != 0 {
		t.Fatalf("a window whose Flush panicked is still in flight: %v", busy)
	}
}
