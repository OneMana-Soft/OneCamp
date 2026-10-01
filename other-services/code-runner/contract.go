// Command code-runner is OneCamp's isolated execution sidecar: it runs one
// short, untrusted program per request in a locked-down environment (no network,
// ephemeral filesystem, hard resource limits) and returns captured output +
// artifacts. It is the ONLY component that executes model/user-authored code;
// the main server never does.
//
// Isolation is layered:
//   - Container level (deployment): the sidecar container runs with
//     network_mode=none, a read-only root fs, a size-capped tmpfs at /work,
//     non-root user, cap_drop=ALL, no-new-privileges, a seccomp profile, a
//     pids-limit, and (recommended) the gVisor runtime. See the code-runner service in
//     final-compose.yml, under the code-execution profile.
//   - Per-run (this process): a fresh work dir per job, POSIX rlimits applied to
//     the child (CPU, address space, processes, file size, open files) via a
//     ulimit shell wrapper, a wall-clock deadline that SIGKILLs the whole
//     process group, and a hard cap on captured output bytes. The work dir is
//     wiped after every run.
//
// The wire contract below MUST stay JSON-compatible with the main server's
// codesandbox package (business/CodeSandbox); it is duplicated here so the
// sidecar is a self-contained, dependency-free deployable.
package main

// Limits are the hard per-run bounds (JSON-compatible with codesandbox.Limits).
// Durations are nanoseconds (Go time.Duration marshals as int64 ns).
type Limits struct {
	Wall          int64 `json:"Wall"`
	CPU           int64 `json:"CPU"`
	MemoryBytes   int64 `json:"MemoryBytes"`
	PIDs          int   `json:"PIDs"`
	OutputBytes   int64 `json:"OutputBytes"`
	MaxArtifacts  int   `json:"MaxArtifacts"`
	ArtifactBytes int64 `json:"ArtifactBytes"`
}

// Job is one unit of work (JSON-compatible with codesandbox.Job).
type Job struct {
	ID       string            `json:"id"`
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Files    map[string][]byte `json:"files,omitempty"`
	Limits   Limits            `json:"limits"`
}

// Artifact is one output (JSON-compatible with codesandbox.Artifact).
type Artifact struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Bytes       []byte `json:"bytes"`
	Kind        string `json:"kind"` // "chart" | "file"
}

// Usage is measured consumption (JSON-compatible with codesandbox.Usage).
type Usage struct {
	WallMS       int64 `json:"wall_ms"`
	CPUMS        int64 `json:"cpu_ms"`
	PeakMemBytes int64 `json:"peak_mem_bytes"`
}

// Result is the response for one Job (JSON-compatible with codesandbox.Result).
type Result struct {
	Status    string     `json:"status"`
	Stdout    string     `json:"stdout,omitempty"`
	Stderr    string     `json:"stderr,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Usage     Usage      `json:"usage"`
}

// Status codes (JSON-compatible with codesandbox.RunStatus).
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusTimeout     = "timeout"
	StatusKilledLimit = "killed_limit"
	StatusOOM         = "oom"
)

// Artifact kinds.
const (
	KindChart = "chart"
	KindFile  = "file"
)
