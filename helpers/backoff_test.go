package helpers

import (
	"testing"
	"time"
)

func TestBackoffDoublesWithinItsCap(t *testing.T) {
	base, max := 200*time.Millisecond, 2*time.Second
	for attempt, low := range map[int]time.Duration{1: 200 * time.Millisecond, 2: 400 * time.Millisecond, 3: 800 * time.Millisecond, 4: 1600 * time.Millisecond} {
		for range 50 {
			d := Backoff(attempt, base, max)
			if d < low || d > low+low/4 || d > max {
				t.Fatalf("attempt %d waited %v, want %v to %v and at most %v", attempt, d, low, low+low/4, max)
			}
		}
	}
	// Far past the cap, and past where base<<attempt overflows, it is the cap.
	for _, attempt := range []int{5, 40, 64, 100} {
		if d := Backoff(attempt, base, max); d != max {
			t.Fatalf("attempt %d waited %v, want the cap %v", attempt, d, max)
		}
	}
	if d := Backoff(0, base, max); d < base || d > base+base/4 {
		t.Fatalf("attempt 0 counts as the first, waited %v", d)
	}
}

// A fixed schedule's wait: its entry for the try, its last past the end, the
// first for anything under one, nothing for no schedule, and the jitter on top
// never more than asked.
func TestScheduleWaitReadsItsTableAndClamps(t *testing.T) {
	s := []time.Duration{10 * time.Second, time.Minute, time.Hour}
	for _, c := range []struct {
		attempt int
		want    time.Duration
	}{{-3, 10 * time.Second}, {0, 10 * time.Second}, {1, 10 * time.Second}, {2, time.Minute}, {3, time.Hour}, {9, time.Hour}} {
		if got := ScheduleWait(s, c.attempt, 0); got != c.want {
			t.Errorf("try %d waits %v, want %v", c.attempt, got, c.want)
		}
	}
	if got := ScheduleWait(nil, 2, 0.25); got != 0 {
		t.Errorf("no schedule waits %v", got)
	}
	for i := 0; i < 200; i++ {
		if got := ScheduleWait(s, 2, 0.25); got < time.Minute || got > time.Minute+15*time.Second {
			t.Fatalf("a quarter's jitter on a minute gave %v", got)
		}
	}
}
