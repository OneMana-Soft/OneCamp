//go:build !linux && !darwin

package helpers

import "errors"

// DiskUsage is not read on this platform; the server only ever runs on Linux.
func DiskUsage(string) (DiskStat, error) {
	return DiskStat{}, errors.New("disk usage is not available on this platform")
}
