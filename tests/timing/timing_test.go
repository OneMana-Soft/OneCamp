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

// A run's CPU time is the time it ran, not the time it waited: a sleep costs
// next to nothing, and work costs at least what the clock says it took on an
// idle core (on Linux, where a thread's CPU time can be read).
func TestFastestCPUCountsOnlyTheTimeItRan(t *testing.T) {
	if _, ok := threadCPU(); !ok {
		t.Skip("no thread CPU time here: FastestCPU is Fastest")
	}
	if slept := FastestCPU(3, func() { time.Sleep(20 * time.Millisecond) }); slept > 5*time.Millisecond {
		t.Errorf("a 20 ms sleep used %v of CPU", slept)
	}
	n := 0
	worked := FastestCPU(3, func() {
		for end := time.Now().Add(15 * time.Millisecond); time.Now().Before(end); {
			n++
		}
	})
	if worked <= 0 || worked > 15*time.Millisecond*2 {
		t.Errorf("15 ms of work used %v of CPU", worked)
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
