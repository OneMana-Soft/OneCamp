package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// enforce.go — the pure, unit-testable enforcement logic: bounding captured
// output, building the resource-limited launch command, classifying artifacts,
// and mapping an exit outcome to a typed status. Kept free of process/FS side
// effects so every case is testable without actually spawning a program.

// cappedWriter is an io.Writer that accepts at most `limit` bytes and then
// reports truncation; further writes are discarded. Used to bound stdout/stderr
// so a run can't flood the response or exfiltrate a large volume via output.
// Safe for concurrent use (stdout+stderr write from separate goroutines).
type cappedWriter struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func newCappedWriter(limit int) *cappedWriter {
	if limit < 0 {
		limit = 0
	}
	return &cappedWriter{limit: limit, buf: make([]byte, 0, min(limit, 64<<10))}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) >= w.limit {
		w.truncated = true
		return len(p), nil // pretend-accept so the child isn't blocked on a full pipe
	}
	room := w.limit - len(w.buf)
	if len(p) > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func (w *cappedWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// buildBootstrap renders a small Python bootstrap that applies POSIX rlimits to
// THIS process via the `resource` module, then runs the user's program in the
// __main__ namespace. Setting limits in Python (not a shell) is portable — it
// doesn't depend on which /bin/sh ships (dash lacks `ulimit -u`) — and precise:
// CPU seconds (RLIMIT_CPU → SIGXCPU), address space (RLIMIT_AS), file size
// (RLIMIT_FSIZE), process count (RLIMIT_NPROC), and open files (RLIMIT_NOFILE).
// Each setrlimit is best-effort (wrapped) so an environment that forbids one
// limit doesn't abort the run before the wall-clock + output caps still apply.
// Deterministic; unit-tested.
func buildBootstrap(l Limits, userFile string) string {
	cpuSec := int64(time.Duration(l.CPU).Seconds())
	if cpuSec < 1 {
		cpuSec = 1
	}
	memBytes := l.MemoryBytes
	if memBytes < 64<<20 {
		memBytes = 64 << 20
	}
	fsize := l.ArtifactBytes
	if fsize < 64<<10 {
		fsize = 64 << 10
	}
	pids := l.PIDs
	if pids < 1 {
		pids = 1
	}
	var b strings.Builder
	b.WriteString("import resource, runpy\n")
	b.WriteString("def _lim(res, val):\n    try:\n        resource.setrlimit(res, (val, val))\n    except Exception:\n        pass\n")
	fmt.Fprintf(&b, "_lim(resource.RLIMIT_CPU, %d)\n", cpuSec)
	fmt.Fprintf(&b, "_lim(resource.RLIMIT_AS, %d)\n", memBytes)
	fmt.Fprintf(&b, "_lim(resource.RLIMIT_FSIZE, %d)\n", fsize)
	fmt.Fprintf(&b, "_lim(resource.RLIMIT_NPROC, %d)\n", pids)
	fmt.Fprintf(&b, "_lim(resource.RLIMIT_NOFILE, %d)\n", 256)
	fmt.Fprintf(&b, "runpy.run_path(%q, run_name='__main__')\n", userFile)
	return b.String()
}

// artifactKind classifies an output file by name: a .json file is a candidate
// chart (the server validates the spec and demotes it to a file if invalid);
// everything else is an opaque file. Pure.
func artifactKind(filename string) string {
	if strings.EqualFold(filepath.Ext(filename), ".json") {
		return KindChart
	}
	return KindFile
}

// contentType returns a best-effort MIME type from the file extension.
func contentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// exitOutcome captures how a child ended, for mapping to a typed status.
type exitOutcome struct {
	timedOut     bool // wall-clock deadline hit
	outputCapped bool // output byte cap hit (we killed it)
	signaled     bool // died from a signal (e.g. SIGKILL — often OOM)
	exitCode     int  // exit code when it exited normally
}

// statusFor maps an exit outcome to a typed RunStatus. Order matters: a
// deadline/output kill is reported as such even though it also manifests as a
// signal.
func statusFor(o exitOutcome) string {
	switch {
	case o.timedOut:
		return StatusTimeout
	case o.outputCapped:
		return StatusKilledLimit
	case o.signaled:
		// A SIGKILL we didn't send is almost always an OOM-kill (cgroup) or the
		// address-space limit; report as OOM so the caller's message is apt.
		return StatusOOM
	case o.exitCode == 0:
		return StatusOK
	default:
		return StatusError
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
