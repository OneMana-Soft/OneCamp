package timing

import (
	"strings"
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

// Ratio runs each pair back to back, a first, and is no less than what each
// pair's own times say, a's taken as at least the floor.
func TestRatioRunsEachPairBackToBack(t *testing.T) {
	var order []string
	r := Ratio(3, time.Millisecond, func() { order = append(order, "a") }, func() {
		order = append(order, "b")
		time.Sleep(3 * time.Millisecond)
	})
	if strings.Join(order, "") != "ababab" || r < 3 {
		t.Errorf("ran %v, ratio %.1f", order, r)
	}
}
