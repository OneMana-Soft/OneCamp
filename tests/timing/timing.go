// Package timing times work in tests run on a machine doing other work: the
// fastest of a few runs is as near the work's own time as can be had, where
// one run's time is mostly the machine's load (a test bound at 3x one run
// failed at a load of 100, and passed alone). A test that can count its work
// instead (reads, steps, probes in flight) counts it; this is for those that
// can't. A test comparing two pieces of work (one size against another, a
// case against a real doc) asks Ratio, which times them back to back.
package timing

import (
	"math"
	"runtime"
	"time"
)

// Fastest runs f runs times, and is the shortest time a run took.
func Fastest(runs int, f func()) time.Duration {
	best := time.Duration(-1)
	for i := 0; i < runs; i++ {
		start := time.Now()
		f()
		if took := time.Since(start); best < 0 || took < best {
			best = took
		}
	}
	return best
}

// Ratio is how many times longer b takes than a: the least, over runs pairs,
// of b's time over a's, each pair run back to back after one collection of
// what came before. What else the machine is doing (its load, its clock,
// which of its cores runs the work) moves both of a pair alike, where the
// fastest of a few runs of each alone may come from different moments: on
// the build machine at a load of 50, the fastest of five readings of each
// grew as much as 27 times from 64 KB to 256 KB of a linear read, and the
// least of five pairs at most 5.5 times (helpers markdownCost_test.go,
// 16 Oct 2026). a's time is taken as at least floor, so a fixed cost too
// quick to time is not read as growth. One collection, not one a pair: in
// a large test binary at load, one a pair took longer than the work.
func Ratio(runs int, floor time.Duration, a, b func()) float64 {
	least := math.Inf(1)
	runtime.GC()
	for i := 0; i < runs; i++ {
		start := time.Now()
		a()
		mid := time.Now()
		b()
		took := time.Since(mid)
		least = min(least, float64(took)/float64(max(mid.Sub(start), floor, time.Nanosecond)))
	}
	return least
}
