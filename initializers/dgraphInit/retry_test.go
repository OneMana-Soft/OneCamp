package dgraphInit

import (
	"testing"
	"time"
)

// Each try waits between half and all of its step, and every wait together
// stays within a request's patience (3.5 s for the eight tries).
func TestAnAbortedWriteWaitsItsTurnWithJitter(t *testing.T) {
	var most time.Duration
	for attempt := 1; attempt < abortRetries; attempt++ {
		step := time.Duration(attempt*attempt) * 25 * time.Millisecond
		seen := map[time.Duration]bool{}
		for i := 0; i < 200; i++ {
			w := abortWait(attempt)
			if w < step/2 || w > step {
				t.Fatalf("try %d waited %v, want %v to %v", attempt, w, step/2, step)
			}
			seen[w] = true
		}
		if len(seen) < 2 {
			t.Errorf("try %d always waited the same: no jitter", attempt)
		}
		most += step
	}
	if most > 3500*time.Millisecond {
		t.Errorf("the tries wait up to %v together", most)
	}
}
