//go:build linux

package timing

import (
	"syscall"
	"time"
)

// rusageThread is RUSAGE_THREAD: the calling thread's own use, not the
// process's (the syscall package names only RUSAGE_SELF and
// RUSAGE_CHILDREN).
const rusageThread = 1

// threadCPU is the CPU time the calling OS thread has used, user and system.
func threadCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(rusageThread, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
