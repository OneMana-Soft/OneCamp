package helpers

import (
	"sync"
	"testing"
	"time"
)

// LatestByKey (latestByKey.go): one run per key at a time, with the value asked last, after a short wait, so a burst of
// collaborative saves writes a board's text once (business/Board's board text, W6-B1).

type ranWith struct {
	key string
	v   int
}

func TestLatestByKeyRunsABurstOnceWithTheLastValue(t *testing.T) {
	got := make(chan ranWith, 8)
	l := NewLatestByKey("test-burst", 30*time.Millisecond, func(k string, v int) { got <- ranWith{k, v} })
	for v := 1; v <= 5; v++ {
		l.Ask("board-a", v)
	}
	if !l.Settle(2 * time.Second) {
		t.Fatal("the burst's run never settled")
	}
	close(got)
	var all []ranWith
	for r := range got {
		all = append(all, r)
	}
	if len(all) != 1 || all[0] != (ranWith{"board-a", 5}) {
		t.Fatalf("five asks ran %v; want ONE run with the last value 5", all)
	}
}

func TestLatestByKeyWaitsBeforeItRuns(t *testing.T) {
	start := time.Now()
	after := make(chan time.Duration, 1)
	l := NewLatestByKey("test-wait", 80*time.Millisecond, func(string, int) { after <- time.Since(start) })
	l.Ask("board-a", 1)
	select {
	case d := <-after:
		if d < 80*time.Millisecond {
			t.Fatalf("ran %v after the ask; want the 80ms wait first", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never ran")
	}
}

func TestLatestByKeyRunsEachKeyWithItsOwnValue(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]int{}
	l := NewLatestByKey("test-keys", 20*time.Millisecond, func(k string, v int) {
		mu.Lock()
		defer mu.Unlock()
		seen[k] = append(seen[k], v)
	})
	l.Ask("board-a", 1)
	l.Ask("board-b", 2)
	l.Ask("board-a", 3)
	if !l.Settle(2 * time.Second) {
		t.Fatal("never settled")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || len(seen["board-a"]) != 1 || seen["board-a"][0] != 3 || len(seen["board-b"]) != 1 || seen["board-b"][0] != 2 {
		t.Fatalf("ran %v; want board-a once with 3 and board-b once with 2", seen)
	}
}

func TestLatestByKeyAnAskDuringARunRunsOnceMoreAfterIt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan int, 4)
	var mu sync.Mutex
	var ran []int
	l := NewLatestByKey("test-during", 10*time.Millisecond, func(_ string, v int) {
		started <- v
		if v == 1 {
			<-release
		}
		mu.Lock()
		ran = append(ran, v)
		mu.Unlock()
	})
	l.Ask("board-a", 1)
	select {
	case v := <-started:
		if v != 1 {
			t.Fatalf("the first run took %d; want 1", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first run never started")
	}
	l.Ask("board-a", 2)
	l.Ask("board-a", 3)
	time.Sleep(60 * time.Millisecond) // well past the wait: nothing may start beside the run in flight
	select {
	case v := <-started:
		t.Fatalf("value %d ran beside the key's run in flight", v)
	default:
	}
	close(release)
	if !l.Settle(2 * time.Second) {
		t.Fatal("never settled")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 1 || ran[1] != 3 {
		t.Fatalf("ran %v; want [1 3]: the asks during the run make ONE more run, with the last value", ran)
	}
}

func TestLatestByKeyAPanickingRunLeavesTheKeyWorking(t *testing.T) {
	got := make(chan int, 2)
	l := NewLatestByKey("test-panic", 10*time.Millisecond, func(_ string, v int) {
		if v == 1 {
			panic("the run broke")
		}
		got <- v
	})
	l.Ask("board-a", 1)
	if !l.Settle(2 * time.Second) {
		t.Fatal("a panicking run never settled")
	}
	l.Ask("board-a", 2)
	select {
	case v := <-got:
		if v != 2 {
			t.Fatalf("ran %d; want 2", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the key never ran again after a run panicked")
	}
}

func TestLatestByKeyForgetsAKeyOnceItsRunsAreDone(t *testing.T) {
	l := NewLatestByKey("test-forget", 5*time.Millisecond, func(string, int) {})
	for i := 0; i < 50; i++ {
		l.Ask(string(rune('a'+i%26))+"-board", i)
	}
	if !l.Settle(2 * time.Second) {
		t.Fatal("never settled")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.keys); n != 0 {
		t.Fatalf("%d keys kept after every run ended; want none (no growth per board ever saved)", n)
	}
}
