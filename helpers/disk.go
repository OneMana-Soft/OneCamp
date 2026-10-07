package helpers

import (
	"context"
	"fmt"
	"os"
)

// How full the server's disk is, for admins and the system check.
//
// A full disk is the worst way for a workspace to fail: Postgres stops
// accepting writes and the workspace hangs rather than breaks, with nothing on
// screen to say why. Self-hosted servers fill up from Docker's leftovers
// (images, build cache, logs) as much as from files people upload, so the
// people who can act (the workspace's admins) are told while there is room.

// DiskStat is how full one filesystem is.
type DiskStat struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	UsedPct    int    `json:"used_pct"`
}

// The same lines `make storage-report` draws: 85% is worth acting on, and near
// 95% Postgres stops accepting writes.
const (
	DiskWarnPct     = 85
	DiskCriticalPct = 95
)

// Level is "ok", "warn" or "critical".
func (d DiskStat) Level() string {
	switch {
	case d.UsedPct >= DiskCriticalPct:
		return "critical"
	case d.UsedPct >= DiskWarnPct:
		return "warn"
	}
	return "ok"
}

// usedPct is df's Use%: used over what a user could use, rounded up, so 85.1%
// reads as 86 the way the server's own tools say it.
func usedPct(usedBytes, availBytes uint64) int {
	if usedBytes+availBytes == 0 {
		return 0
	}
	return int((usedBytes*100 + usedBytes + availBytes - 1) / (usedBytes + availBytes))
}

// DiskPath is what is measured: "/" inside the API's container, which is the
// disk Docker keeps the workspace on. ONECAMP_DISK_PATH points elsewhere when
// the data lives on a disk of its own, mounted into the container.
func DiskPath() string {
	if p := os.Getenv("ONECAMP_DISK_PATH"); p != "" {
		return p
	}
	return "/"
}

// ServerDisk is the disk the workspace lives on.
func ServerDisk() (DiskStat, error) {
	return DiskUsage(DiskPath())
}

// HumanBytes is a size as people say it: "12 GB", "640 MB".
func HumanBytes(b uint64) string {
	const unit = 1000
	if b < unit*unit {
		return fmt.Sprintf("%d KB", b/unit)
	}
	if b < unit*unit*unit {
		return fmt.Sprintf("%d MB", b/(unit*unit))
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(unit*unit*unit))
}

func init() {
	RegisterSystemCheck(SystemCheck{
		Name:     "Disk",
		Kind:     CheckKindDependency,
		Describe: "How full the server's disk is. A full disk stops the database accepting writes, so this warns from 85% and fails from 95%.",
		Probe: func(ctx context.Context) error {
			d, err := ServerDisk()
			if err != nil {
				return SystemCheckNote("could not read the disk: " + err.Error())
			}
			msg := fmt.Sprintf("disk %d%% used, %s free", d.UsedPct, HumanBytes(d.FreeBytes))
			switch d.Level() {
			case "critical":
				return fmt.Errorf("%s: the database stops accepting writes near here. Free room now: remove old files with \"Remove for good\" (Admin, then Archive), and on the server run make housekeeping", msg)
			case "warn":
				return SystemCheckNote(msg + ". Free room soon: remove old files with \"Remove for good\" (Admin, then Archive), and on the server run make housekeeping")
			}
			return nil
		},
	})
}
