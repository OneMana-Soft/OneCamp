package timing

import (
	"testing"
	"time"
)

// Fastest runs the work as many times as asked, and is no shorter than the
// work's own time.
func TestFastestRunsEachTime(t *testing.T) {
	calls := 0
	took := Fastest(3, func() {
		calls++
		time.Sleep(5 * time.Millisecond)
	})
	if calls != 3 || took < 5*time.Millisecond {
		t.Errorf("ran %d times, fastest %v", calls, took)
	}
}
