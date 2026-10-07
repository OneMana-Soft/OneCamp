//go:build linux || darwin

package helpers

import "syscall"

// DiskUsage reads the filesystem that holds path.
func DiskUsage(path string) (DiskStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskStat{}, err
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	used := (st.Blocks - st.Bfree) * bsize
	avail := st.Bavail * bsize
	return DiskStat{TotalBytes: total, FreeBytes: avail, UsedPct: usedPct(used, avail)}, nil
}
