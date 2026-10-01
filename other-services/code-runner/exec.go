package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// exec.go — running one job under per-run isolation. Container-level isolation
// (no network, ro rootfs, non-root, seccomp, gVisor) is provided by how the
// sidecar container is deployed; this adds the per-run bounds: a fresh work dir,
// rlimits on the child, a wall-clock deadline that SIGKILLs the process group,
// a capped output buffer, and a workdir wipe.

// workRoot is the (tmpfs-backed) directory under which each run gets an isolated
// subdir. Overridable via CODE_RUNNER_WORKDIR; defaults to /work.
func workRoot() string {
	if v := strings.TrimSpace(os.Getenv("CODE_RUNNER_WORKDIR")); v != "" {
		return v
	}
	return "/work"
}

// runJob executes one job and returns its Result. It never returns an error to
// the caller for a run that merely failed/was-killed — those are encoded in
// Result.Status; it returns an error only for a setup failure (couldn't create
// the work dir, etc.), which the server maps to a 500.
func runJob(job Job) (Result, error) {
	start := time.Now()

	dir, err := os.MkdirTemp(workRoot(), "run-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir) // wipe all run state afterward

	outDir := filepath.Join(dir, "out")
	dataDir := filepath.Join(dir, "data")
	for _, d := range []string{outDir, dataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Result{}, err
		}
	}

	// Write injected input files under data/ (read-only intent; the sandbox is
	// already non-root on a ro rootfs, so these are the only readable inputs).
	for name, content := range job.Files {
		// Guard against path traversal in a file name.
		clean := filepath.Base(filepath.Clean("/" + name))
		if clean == "." || clean == "/" || clean == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dataDir, clean), content, 0o400); err != nil {
			return Result{}, err
		}
	}

	// Write the user's program and a bootstrap that applies rlimits (in Python,
	// no shell) then runs it. The server-injected preamble loads inputs from the
	// relative ./data dir; CWD is set to the run dir below so that resolves, and
	// programs write outputs to ./out (collected as artifacts).
	userFile := filepath.Join(dir, "user.py")
	if err := os.WriteFile(userFile, []byte(job.Code), 0o400); err != nil {
		return Result{}, err
	}
	bootstrapFile := filepath.Join(dir, "bootstrap.py")
	if err := os.WriteFile(bootstrapFile, []byte(buildBootstrap(job.Limits, userFile)), 0o400); err != nil {
		return Result{}, err
	}

	wall := time.Duration(job.Limits.Wall)
	if wall <= 0 {
		wall = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), wall)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", bootstrapFile)
	cmd.Dir = dir
	// Minimal, secret-free environment. MPLBACKEND=Agg lets matplotlib render
	// headless; DATA_DIR/OUT_DIR let a program locate inputs/outputs portably.
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"MPLBACKEND=Agg",
		"DATA_DIR=" + dataDir,
		"OUT_DIR=" + outDir,
		"PYTHONDONTWRITEBYTECODE=1",
	}
	// New process group so a timeout kill takes down any children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := newCappedWriter(int(job.Limits.OutputBytes))
	errBuf := newCappedWriter(int(maxInt64(job.Limits.OutputBytes/2, 64<<10)))
	cmd.Stdout = out
	cmd.Stderr = errBuf

	runErr := cmd.Run()

	outcome := exitOutcome{
		timedOut:     ctx.Err() == context.DeadlineExceeded,
		outputCapped: out.Truncated(),
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				outcome.signaled = ws.Signaled()
				outcome.exitCode = ws.ExitStatus()
			} else {
				outcome.exitCode = ee.ExitCode()
			}
		} else {
			outcome.exitCode = 1
		}
	}

	// Ensure the whole group is dead (best-effort) on timeout/cap.
	if cmd.Process != nil && (outcome.timedOut || outcome.outputCapped) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	res := Result{
		Status: statusFor(outcome),
		Stdout: out.String(),
		Stderr: errBuf.String(),
		Usage: Usage{
			WallMS: time.Since(start).Milliseconds(),
			CPUMS:  cpuMillis(cmd),
		},
	}

	// Collect artifacts only on a clean finish (a killed run's out/ is untrusted
	// / partial).
	if res.Status == StatusOK {
		res.Artifacts = collectArtifacts(outDir, job.Limits)
	}
	return res, nil
}

// collectArtifacts reads out/ files into artifacts, bounded by MaxArtifacts,
// per-artifact ArtifactBytes, and a combined budget shared with stdout
// (OutputBytes). Deterministic order (sorted by name). Oversized files are
// skipped rather than truncated (a partial CSV/image is worse than none).
func collectArtifacts(outDir string, l Limits) []Artifact {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	maxArtifacts := l.MaxArtifacts
	if maxArtifacts <= 0 {
		maxArtifacts = 8
	}
	perArtifact := l.ArtifactBytes
	if perArtifact <= 0 {
		perArtifact = 2 << 20
	}
	budget := l.OutputBytes
	if budget <= 0 {
		budget = 4 << 20
	}

	var out []Artifact
	for _, name := range names {
		if len(out) >= maxArtifacts || budget <= 0 {
			break
		}
		path := filepath.Join(outDir, name)
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() > perArtifact || info.Size() > budget {
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		budget -= int64(len(data))
		out = append(out, Artifact{
			Name:        name,
			ContentType: contentType(name),
			Bytes:       data,
			Kind:        artifactKind(name),
		})
	}
	return out
}

// cpuMillis returns the child's consumed CPU time in ms, best-effort (0 when
// unavailable).
func cpuMillis(cmd *exec.Cmd) int64 {
	if cmd.ProcessState == nil {
		return 0
	}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
		sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
		return (user + sys).Milliseconds()
	}
	return 0
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
