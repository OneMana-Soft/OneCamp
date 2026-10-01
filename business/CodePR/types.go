// Package codepr is the code-PR agent: given a natural-language task on a linked
// repository, it drives a change to a verified, reviewable pull request —
// reliably and irrespective of repo size — on the operator's own infrastructure.
//
// This package is the MAIN-SERVER, trusted decision layer. It never executes
// untrusted repo code itself: the actual clone/edit/build/push happens in the
// isolated `code-runner` sidecar behind the CodingRunner interface (a mock
// implementation lets the entire decision layer be proven without any real
// execution). Design: .kiro/specs/agent-code-pr.
//
// Trust + safety invariants (enforced across the package + runner):
//   - Human-review-only: a run ends at a PR on a FRESH branch; it never merges
//     and never pushes to a protected/base branch.
//   - Contained: the runner reaches only the target repo (single-repo token) and
//     the configured git host; no workspace DB/secrets/other repos/open internet.
//   - Verified-or-honest: a non-draft PR implies the repo's own
//     format/lint/build/tests passed; otherwise the run reports honestly.
//   - Size-agnostic: checkout is shallow+partial+sparse and the model only ever
//     sees retrieval-scoped files or one bounded selector shard.
package codepr

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Coding-run terminal/step statuses. A durable caller switches on these to
// choose the right transition + user-facing message; they are STABLE codes, not
// display strings.
const (
	StatusOK          = "ok"          // a PR was opened (verified, unless draft)
	StatusNoGreen     = "no_green"    // couldn't reach a passing build/tests
	StatusTooLarge    = "too_large"   // repo/task exceeded checkout/context limits
	StatusBlocked     = "blocked"     // needs_human (ambiguous repo, missing token, a decision)
	StatusTimeout     = "timeout"     // wall-clock/limit kill
	StatusError       = "error"       // unexpected failure (sanitized message)
	StatusUnavailable = "unavailable" // runner down/misconfigured/egress-blocked
)

// SurfaceKind identifies where a coding run posts its evolving progress + result.
// Kept as a small local type so this package stays a leaf (business/AIAgent maps
// its own Surface onto this; codepr never imports AIAgent, avoiding a cycle).
type SurfaceKind string

const (
	SurfaceChannelPost SurfaceKind = "channel_post" // in-thread comment on a post
	SurfaceDM          SurfaceKind = "dm"
	SurfaceGroupChat   SurfaceKind = "group_chat"
	SurfaceTask        SurfaceKind = "task" // a project task's agent-activity thread
)

// Surface is the minimal reply-surface descriptor the progress/result path needs.
type Surface struct {
	Kind      SurfaceKind `json:"kind"`
	ChannelID string      `json:"channel_id,omitempty"`
	PostID    string      `json:"post_id,omitempty"`
	MessageID string      `json:"message_id,omitempty"`
	TaskID    string      `json:"task_id,omitempty"`
}

// RepoRef is a linked repository resolved to its exact owner/name (never a bare
// name or a global-search result — see agentGithubContext).
type RepoRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// FullName renders "owner/name" for git/GitHub calls.
func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

// Valid reports whether both parts are present.
func (r RepoRef) Valid() bool { return r.Owner != "" && r.Name != "" }

// PRCreateURL returns the GitHub "open a pull request" web URL for a pushed head
// branch, so a human can open the PR in one click when the automated open step
// didn't run (branch pushed but PR not opened, or a WIP branch left for review).
// base "" targets the repo's default branch. Empty when repo/branch is unknown.
// (v1 targets github.com, matching the rest of the GitHub integration.)
func (r RepoRef) PRCreateURL(base, head string) string {
	head = strings.TrimSpace(head)
	if !r.Valid() || head == "" {
		return ""
	}
	if b := strings.TrimSpace(base); b != "" {
		return "https://github.com/" + r.Owner + "/" + r.Name + "/compare/" + b + "..." + head + "?expand=1"
	}
	return "https://github.com/" + r.Owner + "/" + r.Name + "/pull/new/" + head
}

// BranchURL returns the web URL of a pushed branch — a fallback link when a PR
// couldn't be opened, so the change is never stranded without a way to see it.
// Empty when repo/branch is unknown.
func (r RepoRef) BranchURL(branch string) string {
	branch = strings.TrimSpace(branch)
	if !r.Valid() || branch == "" {
		return ""
	}
	return "https://github.com/" + r.Owner + "/" + r.Name + "/tree/" + branch
}

// Task is one unit of code-PR work: change ONE repo to accomplish ONE task,
// ending in a PR, a needs_human pause, or an honest failure. Runs AS the agent
// owner (permissions re-checked by the tools), on behalf of TriggeredBy (the
// human to notify).
type Task struct {
	AgentID     uuid.UUID `json:"agent_id"`
	OwnerUserID uuid.UUID `json:"owner_user_id"`          // permissions the run uses
	TriggeredBy uuid.UUID `json:"triggered_by,omitempty"` // human to notify (zero for schedule/event)
	Repo        RepoRef   `json:"repo"`
	Instruction string    `json:"instruction"` // the condensed intake prompt
	BaseBranch  string    `json:"base_branch"` // empty => the repo's default branch
	Surface     Surface   `json:"surface"`     // where progress/result post
	WholeRepo   bool      `json:"whole_repo"`  // triggers the selector (MapReduce) path
	// Display names for PR provenance (not identity — permissions use the IDs).
	AgentDisplayName string `json:"agent_display_name,omitempty"`
	RequestedByName  string `json:"requested_by_name,omitempty"`
	// Continue is the agent's own pull request on this thread or task. When
	// GitHub confirms it is still open on the agent's branch, the run pushes a
	// commit to it instead of opening another (see continue.go).
	Continue *PriorPR `json:"continue,omitempty"`
	// AgentTaskID is the durable job running this task, recorded on the audit
	// row so a later follow-up can find the pull request this run opened.
	AgentTaskID uuid.UUID `json:"agent_task_id,omitempty"`
}

// SelectorKind is the language of a deterministic whole-repo selector.
type SelectorKind string

const (
	SelectorLexical     SelectorKind = "lexical"      // ripgrep / literal-or-regex
	SelectorTreeSitter  SelectorKind = "treesitter"   // syntax-node query
	SelectorImportGraph SelectorKind = "import_graph" // reference/import traversal
)

// Selector is a model-authored, deterministically-executable relevance test that
// enumerates a finite candidate set over the whole repo (Req 6). It is an
// inspectable audit artifact, not an opaque "I looked everywhere".
type Selector struct {
	Kind     SelectorKind `json:"kind"`
	Query    string       `json:"query"`
	Language string       `json:"language,omitempty"`
}

// VerifierKind classifies one quality gate.
type VerifierKind string

const (
	VerifierFormat VerifierKind = "fmt"
	VerifierLint   VerifierKind = "lint"
	VerifierBuild  VerifierKind = "build"
	VerifierTest   VerifierKind = "test"
)

// VerifierResult is one gate's outcome, with output already SUMMARIZED (the
// agent never parses raw build logs; the runner extracts the essential lines).
type VerifierResult struct {
	Name    string       `json:"name"`
	Kind    VerifierKind `json:"kind"`
	Passed  bool         `json:"passed"`
	Summary string       `json:"summary"` // short success text or the essential error lines
}

// VerifierReport is the full gate outcome for a run. HadTests distinguishes
// "tests passed" from "no tests to run" so the PR body can disclose it honestly.
type VerifierReport struct {
	Ran       []VerifierResult `json:"ran"`
	AllPassed bool             `json:"all_passed"`
	HadTests  bool             `json:"had_tests"`
}

// DiffStat summarizes the produced change without inlining a huge diff.
type DiffStat struct {
	Files        int  `json:"files"`
	Added        int  `json:"added"`
	Removed      int  `json:"removed"`
	PartialScope bool `json:"partial_scope"` // working set exceeded budget; scoped + disclosed
}

// CodingUsage is the metered resource cost of a run (for budgets + audit).
type CodingUsage struct {
	WallMS           int64 `json:"wall_ms"`
	CPUMS            int64 `json:"cpu_ms"`
	PeakMemBytes     int64 `json:"peak_mem_bytes"`
	VerifyIterations int   `json:"verify_iterations"`
}

// CodingLimits bounds one run so a task can neither hang nor run away. Enforced
// in the runner (OS-level) and honored by the orchestrator.
type CodingLimits struct {
	Wall           time.Duration `json:"wall"`
	CPU            time.Duration `json:"cpu"`
	MemoryBytes    int64         `json:"memory_bytes"`
	PIDs           int           `json:"pids"`
	DiskBytes      int64         `json:"disk_bytes"`   // checkout + build scratch cap
	OutputBytes    int64         `json:"output_bytes"` // captured stdout/diff cap
	CloneTimeout   time.Duration `json:"clone_timeout"`
	MaxVerifyIters int           `json:"max_verify_iters"`
	MaxCloneDepth  int           `json:"max_clone_depth"`
}

// Coding-run TIMING — the single source of truth every layer derives its own
// deadline from. A coding run is the longest thing the platform executes, and
// three independently-chosen literals (the in-sandbox wall limit, the durable
// queue's lease TTL, the sidecar's HTTP write deadline) is exactly how a live
// run gets reclaimed by a second worker or cut off mid-push: duplicate work,
// conflicting pushes, wrong status. Everything downstream must derive from
// CodingWall / MaxRunWallClock instead of restating a number.
const (
	// DefaultCodingWallMinutes is the in-sandbox wall-clock ceiling for ONE
	// coding run when nothing is configured. Large enough for a real clone +
	// edit/verify loop + push on a production repository, small enough that a
	// wedged run resolves within a coffee break. It matches the DEFAULT of the
	// ai_settings.code_pr_wall_minutes column (migration 133) so the DB default,
	// this fallback, and the "0 = use the built-in default" setting all agree.
	DefaultCodingWallMinutes = 30
	// MinCodingWallMinutes / MaxCodingWallMinutes bound an admin or operator
	// value so a typo can neither starve a run (30s) nor pin a worker for a day.
	// Exported so the admin API validates against these exact bounds instead of
	// restating them.
	MinCodingWallMinutes = 2
	MaxCodingWallMinutes = 60
	// CodingWallEnvVar is the boot-time escape hatch for deployments that never
	// open the admin UI, documented the same way AI_CODE_PR_ALLOW_UNLINKED is: it
	// applies only when the admin setting is unset (0). Set it ONCE and every
	// derived deadline (lease TTL, sidecar write timeout) follows; the sidecar is
	// a separate module, so it reads the same variable with the same bounds.
	CodingWallEnvVar = "AI_CODE_PR_WALL_MINUTES"
	// CodingDispatchOverhead is the head-room around the in-sandbox wall limit
	// for the trusted work that brackets it: repo resolve, budget gate, token
	// mint, the HTTP round trip and result decode, the scope judge, and opening
	// the PR. A run therefore occupies its caller for at most
	// CodingWall + CodingDispatchOverhead.
	CodingDispatchOverhead = 5 * time.Minute
)

// configuredCodingWallMinutes caches the ADMIN-configured wall limit so
// CodingWall stays a pure in-memory read: it is called from synchronous timing
// paths (the durable queue's lease TTL, the sidecar transport deadline, limit
// construction) that must never perform a DB read. Zero means "not loaded, or
// explicitly unset", in which case CodingWall falls back to the env override and
// then the built-in default — so every call before the first settings load is
// safe. Refreshed by SetConfiguredCodingWall whenever settings are loaded (see
// NewLiveOrchestrator) or saved (the admin config setter).
var configuredCodingWallMinutes atomic.Int32

// SetConfiguredCodingWall publishes the admin-configured wall limit (in minutes)
// as the effective ceiling for subsequent runs and returns the duration now in
// force. A value of 0 (or negative) CLEARS the setting, restoring the env
// override / built-in default; any other value is clamped to
// [MinCodingWallMinutes, MaxCodingWallMinutes] so a bad value can never widen or
// void the limit. Concurrency-safe: callers may refresh it while runs are in
// flight, and an in-flight run keeps the limits it was built with.
func SetConfiguredCodingWall(minutes int) time.Duration {
	if minutes <= 0 {
		configuredCodingWallMinutes.Store(0)
	} else {
		configuredCodingWallMinutes.Store(int32(ClampCodingWallMinutes(minutes)))
	}
	return CodingWall()
}

// ClampCodingWallMinutes bounds a wall-limit value to
// [MinCodingWallMinutes, MaxCodingWallMinutes], passing 0 (and anything
// negative) through as 0 = "unset, use the default". Pure and total, so the
// admin layer can validate/normalize with the exact bounds enforced here.
func ClampCodingWallMinutes(minutes int) int {
	if minutes <= 0 {
		return 0
	}
	if minutes < MinCodingWallMinutes {
		return MinCodingWallMinutes
	}
	if minutes > MaxCodingWallMinutes {
		return MaxCodingWallMinutes
	}
	return minutes
}

// CodingWall returns the in-sandbox wall-clock ceiling for one coding run,
// resolved in precedence order: the admin setting (when set), then the
// CodingWallEnvVar override, then DefaultCodingWallMinutes — always clamped to
// [MinCodingWallMinutes, MaxCodingWallMinutes]. Safe by construction: an unset,
// unparseable or out-of-range value falls back to a sane bound rather than
// disabling the limit, and the read never touches the database.
func CodingWall() time.Duration {
	minutes := DefaultCodingWallMinutes
	if v := strings.TrimSpace(os.Getenv(CodingWallEnvVar)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	if configured := int(configuredCodingWallMinutes.Load()); configured > 0 {
		minutes = configured
	}
	if minutes < MinCodingWallMinutes {
		minutes = MinCodingWallMinutes
	}
	if minutes > MaxCodingWallMinutes {
		minutes = MaxCodingWallMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// MaxRunWallClock returns the longest ONE coding job can legitimately occupy its
// caller, end to end: the in-sandbox wall ceiling plus the trusted-layer
// overhead around it. Any OUTER deadline (a durable queue's lease TTL, the
// sidecar's HTTP write timeout, a transport ceiling) must be at least this, or a
// perfectly healthy maximum-length run is cut off or double-run. Callers add
// their own margin on top; they never restate the wall limit.
func MaxRunWallClock() time.Duration {
	return CodingWall() + CodingDispatchOverhead
}

// defaultCodingLimits returns conservative, production-safe defaults. An admin's
// configured budgets bound wall-clock further; these are the per-run backstops.
// Wall comes from CodingWall (the shared source of truth), never a literal.
func defaultCodingLimits() CodingLimits {
	return CodingLimits{
		Wall:           CodingWall(),
		CPU:            10 * time.Minute,
		MemoryBytes:    4 << 30, // 4 GiB
		PIDs:           512,
		DiskBytes:      8 << 30, // 8 GiB
		OutputBytes:    4 << 20, // 4 MiB captured
		CloneTimeout:   5 * time.Minute,
		MaxVerifyIters: 6,
		MaxCloneDepth:  1,
	}
}

// withWall returns the limits with wall as the wall-clock ceiling, keeping the
// inner budgets coherent: neither the CPU budget nor the clone timeout may
// exceed the wall they live inside, so a short configured wall can't hand the
// runner a clone timeout it could never reach. A non-positive wall leaves the
// limits untouched (the caller's already-resolved default stands).
func (l CodingLimits) withWall(wall time.Duration) CodingLimits {
	if wall <= 0 {
		return l
	}
	l.Wall = wall
	if l.CPU > wall {
		l.CPU = wall
	}
	if l.CloneTimeout > wall {
		l.CloneTimeout = wall
	}
	return l
}

// EgressPolicy is default-deny with an explicit host allowlist (the git host,
// plus any admin-allowlisted package registry the build needs). Enforced at the
// network-namespace/proxy level in the runner, never by in-process policy.
type EgressPolicy struct {
	AllowHosts []string `json:"allow_hosts"`
}

// CodingJob is the self-contained unit handed to the runner. The CloneToken is a
// short-lived, single-repo credential injected only for clone/push; it is never
// written into the checkout and never logged.
type CodingJob struct {
	ID         string  `json:"id"`
	Repo       RepoRef `json:"repo"`
	CloneToken string  `json:"-"` // secret; never serialized to logs
	BaseBranch string  `json:"base_branch"`
	HeadBranch string  `json:"head_branch"` // fresh and unique, or the branch being continued
	// ContinueBranch: HeadBranch already exists (the agent's open pull request);
	// clone it and add a commit instead of branching from BaseBranch.
	ContinueBranch bool         `json:"continue_branch,omitempty"`
	Prompt         string       `json:"prompt"`
	SparsePaths    []string     `json:"sparse_paths,omitempty"`
	Selectors      []Selector   `json:"selectors,omitempty"`
	Limits         CodingLimits `json:"limits"`
	Egress         EgressPolicy `json:"egress"`
}

// CodingResult is what the runner returns: the pushed branch + a stored diff
// reference (never necessarily inlined) + the verifier report + usage + a
// sanitized message on any non-ok status. Status is one of the Status* codes.
type CodingResult struct {
	Status        string         `json:"status"`
	HeadBranch    string         `json:"head_branch,omitempty"`
	DiffRef       string         `json:"diff_ref,omitempty"` // stored object/attachment, not the raw diff
	Diff          string         `json:"diff,omitempty"`     // bounded inline unified diff (for the scope judge + FE preview)
	DiffStat      DiffStat       `json:"diff_stat"`
	Verifier      VerifierReport `json:"verifier"`
	Selectors     []Selector     `json:"selectors,omitempty"`      // echoed for audit (whole-repo)
	CandidateList []string       `json:"candidate_list,omitempty"` // echoed for audit (whole-repo)
	Usage         CodingUsage    `json:"usage"`
	Message       string         `json:"message,omitempty"` // sanitized honest message on non-ok
}

// CodingRunner executes one CodingJob in an isolated environment and returns the
// result. It is an interface so the backend is swappable (mock for tests, the
// real sidecar in production) and the entire decision layer is provable without
// any real clone/edit/build/push. Implementations MUST NOT leak host paths, the
// clone token, or secrets in CodingResult.Message.
type CodingRunner interface {
	Run(ctx context.Context, job CodingJob) (CodingResult, error)
}
