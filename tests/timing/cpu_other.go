//go:build !linux

package timing

import "time"

// threadCPU can't read a thread's CPU time here: FastestCPU falls back to
// wall time.
func threadCPU() (time.Duration, bool) { return 0, false }
