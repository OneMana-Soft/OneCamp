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
