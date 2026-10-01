package ai

// System resource awareness for the admin AI panel.
//
// Pulling a local model can be tens of GB. Before an admin clicks
// "install", the panel shows the server's free disk, total/used RAM, and
// CPU count so they can judge whether a model will fit and run acceptably
// on CPU. This is advisory, not a hard gate.
//
// Topology note (single-tenant, self-hosted): in OneCamp's default
// compose, Ollama's model volume (./data/ollama) and the Go service's
// working data (./data) live on the SAME host disk, so the Go process's
// view of free space on the data path is representative of where models
// land. AI_DISK_PATH lets an operator point the probe at the real model
// volume if they've split disks.

import (
	"context"
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// SystemStats is a snapshot of server resources relevant to running and
// installing local models.
type SystemStats struct {
	// Disk (for the model storage path).
	DiskPath        string  `json:"disk_path"`
	DiskTotalBytes  uint64  `json:"disk_total_bytes"`
	DiskFreeBytes   uint64  `json:"disk_free_bytes"`
	DiskUsedPercent float64 `json:"disk_used_percent"`

	// Memory.
	MemTotalBytes  uint64  `json:"mem_total_bytes"`
	MemFreeBytes   uint64  `json:"mem_available_bytes"`
	MemUsedPercent float64 `json:"mem_used_percent"`

	// CPU.
	CPUCount   int     `json:"cpu_count"`
	CPUUsedPct float64 `json:"cpu_used_percent,omitempty"`
	GoMaxProcs int     `json:"go_max_procs"`

	// Errors collecting any individual stat are reported here without
	// failing the whole snapshot.
	Warnings []string `json:"warnings,omitempty"`
}

// diskPath returns the path whose filesystem we report free space for.
//
// Accuracy note: the Go service runs in its own container. Its root
// filesystem (and /app) is the Docker overlay, which in a single-host
// deploy is physically backed by the same disk as ./data/ollama — so the
// free-space figure is a reasonable proxy for "will this model fit."
// For split-disk setups, mount the model volume into the go-service and
// set AI_DISK_PATH to that mount for an exact reading.
func diskPath() string {
	if p := os.Getenv("AI_DISK_PATH"); p != "" {
		return p
	}
	return "/"
}

// GetSystemStats collects a best-effort resource snapshot. Individual
// probe failures are recorded as warnings rather than failing the call,
// so the admin always gets whatever could be read.
func GetSystemStats(ctx context.Context) *SystemStats {
	s := &SystemStats{
		DiskPath:   diskPath(),
		CPUCount:   runtime.NumCPU(),
		GoMaxProcs: runtime.GOMAXPROCS(0),
	}

	if du, err := disk.UsageWithContext(ctx, s.DiskPath); err == nil {
		s.DiskTotalBytes = du.Total
		s.DiskFreeBytes = du.Free
		s.DiskUsedPercent = du.UsedPercent
	} else {
		s.Warnings = append(s.Warnings, "disk usage unavailable: "+err.Error())
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemTotalBytes = vm.Total
		s.MemFreeBytes = vm.Available
		s.MemUsedPercent = vm.UsedPercent
	} else {
		s.Warnings = append(s.Warnings, "memory stats unavailable: "+err.Error())
	}

	// A short CPU sample would block ~200ms; skip it here and let the FE
	// poll a dedicated endpoint if it wants live CPU. Report logical count
	// only, which is the number that matters for model feasibility.
	if pcts, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pcts) > 0 {
		s.CPUUsedPct = pcts[0]
	}

	return s
}
