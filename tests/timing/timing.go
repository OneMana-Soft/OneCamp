// Package timing times work in tests run on a machine doing other work: the
// fastest of a few runs is as near the work's own time as can be had, where
// one run's time is mostly the machine's load (a test bound at 3x one run
// failed at a load of 100, and passed alone). A test that can count its work
// instead (reads, steps, probes in flight) counts it; this is for those that
// can't.
package timing

import "time"

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
