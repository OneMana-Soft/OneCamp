// Package codesandbox is OneCamp's bounded, network-less code-execution
// capability for data analysis and artifact generation ("run the numbers",
// render a chart, transform a file).
//
// TRUST BOUNDARY
// --------------
// This package (running in the main server) NEVER executes untrusted code
// itself. It defines the contract between the trusted server and an isolated,
// disposable `code-runner` sidecar that actually runs code with no network, no
// credentials, an ephemeral filesystem, and hard OS-level resource limits. The
// server side only: resolves permission-checked inputs, gates budgets, submits
// a self-contained Job, classifies the returned artifacts, and audits — see the
// spec at .kiro/specs/agent-code-sandbox.
//
// The Runner interface abstracts the isolation backend (a container-per-run
// sidecar today, a WASM runtime later) so it is swappable and unit-testable via
// MockRunner without any Docker or code execution.
package codesandbox

import (
	"context"
	"time"
)

// Language is a supported interpreter. Kept as a closed set so a Job can never
// ask the runner to launch an arbitrary binary.
type Language string

const (
	// LanguagePython is the initial (and, for v1, only) supported language —
	// the runner image ships a pinned Python + a curated data-analysis library
	// set (numpy/pandas/matplotlib), with no run-time package installation.
	LanguagePython Language = "python"
)

// ValidLanguage reports whether l is a supported interpreter.
func ValidLanguage(l Language) bool {
	switch l {
	case LanguagePython:
		return true
	default:
		return false
	}
}

// Limits are the hard, per-run bounds the runner enforces at the OS level. Every
// dimension is capped so no single run can exhaust the host or exfiltrate a
// large volume of data through its output. A breach kills the run with a typed
// RunStatus (see StatusTimeout / StatusKilledLimit / StatusOOM).
type Limits struct {
	Wall          time.Duration // wall-clock hard kill
	CPU           time.Duration // CPU-time limit
	MemoryBytes   int64         // memory ceiling (OOM-kill above)
	PIDs          int           // max processes/threads (fork-bomb guard)
	OutputBytes   int64         // combined stdout + artifact bytes cap
	MaxArtifacts  int           // max number of returned artifacts
	ArtifactBytes int64         // per-artifact byte cap
}

// Default limit values. Chosen to comfortably run a data-analysis script while
// keeping a single run cheap and bounded. Admin config may lower (never raise
// past the safety ceilings enforced by ClampLimits).
const (
	defaultWall          = 30 * time.Second
	defaultCPU           = 20 * time.Second
	defaultMemoryBytes   = 512 << 20 // 512 MiB
	defaultPIDs          = 64
	defaultOutputBytes   = 4 << 20 // 4 MiB
	defaultMaxArtifacts  = 8
	defaultArtifactBytes = 2 << 20 // 2 MiB
)

// Safety ceilings: even an admin-supplied Limits is clamped to these so a
// misconfiguration can't hand a run an unbounded budget.
const (
	maxWall          = 5 * time.Minute
	maxCPU           = 4 * time.Minute
	maxMemoryBytes   = 2 << 30 // 2 GiB
	maxPIDs          = 512
	maxOutputBytes   = 32 << 20 // 32 MiB
	maxMaxArtifacts  = 32
	maxArtifactBytes = 16 << 20 // 16 MiB
)

// DefaultLimits returns the standard per-run limits.
func DefaultLimits() Limits {
	return Limits{
		Wall:          defaultWall,
		CPU:           defaultCPU,
		MemoryBytes:   defaultMemoryBytes,
		PIDs:          defaultPIDs,
		OutputBytes:   defaultOutputBytes,
		MaxArtifacts:  defaultMaxArtifacts,
		ArtifactBytes: defaultArtifactBytes,
	}
}

// ClampLimits fills zero/negative fields from DefaultLimits and caps every field
// at its safety ceiling, so the value handed to the runner is always sane and
// bounded regardless of caller/admin input. Pure and total.
func ClampLimits(l Limits) Limits {
	d := DefaultLimits()
	if l.Wall <= 0 {
		l.Wall = d.Wall
	}
	if l.CPU <= 0 {
		l.CPU = d.CPU
	}
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = d.MemoryBytes
	}
	if l.PIDs <= 0 {
		l.PIDs = d.PIDs
	}
	if l.OutputBytes <= 0 {
		l.OutputBytes = d.OutputBytes
	}
	if l.MaxArtifacts <= 0 {
		l.MaxArtifacts = d.MaxArtifacts
	}
	if l.ArtifactBytes <= 0 {
		l.ArtifactBytes = d.ArtifactBytes
	}
	l.Wall = minDuration(l.Wall, maxWall)
	l.CPU = minDuration(l.CPU, maxCPU)
	l.MemoryBytes = minInt64(l.MemoryBytes, maxMemoryBytes)
	l.PIDs = minInt(l.PIDs, maxPIDs)
	l.OutputBytes = minInt64(l.OutputBytes, maxOutputBytes)
	l.MaxArtifacts = minInt(l.MaxArtifacts, maxMaxArtifacts)
	l.ArtifactBytes = minInt64(l.ArtifactBytes, maxArtifactBytes)
	return l
}

// Job is the self-contained unit of work submitted to the runner. It carries
// everything the sandbox needs and NOTHING it shouldn't: no credentials, no
// tokens, no network references. Files are injected read-only into the sandbox
// (e.g. /data/<name>) and are the ONLY data the code can see.
type Job struct {
	ID       string            `json:"id"`
	Language Language          `json:"language"`
	Code     string            `json:"code"`
	Files    map[string][]byte `json:"files,omitempty"`
	Limits   Limits            `json:"limits"`
}

// ArtifactKind distinguishes a chart spec (rendered natively via the existing
// chart embed) from an opaque file (stored as an attachment).
type ArtifactKind string

const (
	// ArtifactChart is a JSON chart spec the frontend renders as an inline SVG
	// chart (see the chart embed / AgentChart renderer).
	ArtifactChart ArtifactKind = "chart"
	// ArtifactFile is any other output (CSV, PNG, …) stored as an attachment.
	ArtifactFile ArtifactKind = "file"
)

// Artifact is one output produced by a run.
type Artifact struct {
	Name        string       `json:"name"`
	ContentType string       `json:"content_type"`
	Bytes       []byte       `json:"bytes"`
	Kind        ArtifactKind `json:"kind"`
}

// RunStatus is the stable, typed outcome of a run. Callers switch on this rather
// than parsing messages.
type RunStatus string

const (
	StatusOK          RunStatus = "ok"           // ran to completion
	StatusError       RunStatus = "error"        // user code raised / non-zero exit
	StatusTimeout     RunStatus = "timeout"      // wall-clock deadline hit
	StatusKilledLimit RunStatus = "killed_limit" // CPU/PID/output/artifact cap hit
	StatusOOM         RunStatus = "oom"          // memory ceiling hit
)

// Succeeded reports whether the run produced a usable result.
func (s RunStatus) Succeeded() bool { return s == StatusOK }

// Usage is the measured resource consumption of a run, metered against the
// sandbox budget and recorded for audit.
type Usage struct {
	WallMS       int64 `json:"wall_ms"`
	CPUMS        int64 `json:"cpu_ms"`
	PeakMemBytes int64 `json:"peak_mem_bytes"`
}

// Result is the runner's response for one Job. Stderr, when present, is limited
// to the user's own code frames (host paths stripped) before it leaves the
// runner.
type Result struct {
	Status    RunStatus  `json:"status"`
	Stdout    string     `json:"stdout,omitempty"`
	Stderr    string     `json:"stderr,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Usage     Usage      `json:"usage"`
}

// Runner executes a Job in isolation and returns its Result. Implementations:
//   - the container-per-run sidecar client (production),
//   - MockRunner (tests; no execution).
//
// A non-nil error means the run could not be attempted (runner down, transport
// error); a run that executed but failed/was-killed returns a nil error with a
// non-OK Result.Status, so callers distinguish "couldn't run" from "ran and
// failed".
type Runner interface {
	Run(ctx context.Context, job Job) (Result, error)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
