package main

// coding_contract.go — the wire contract for the CODE-PR coding profile
// (POST /code-run), kept JSON-compatible with the main server's business/CodePR
// package (codingJobWire + CodingResult + friends). Like contract.go for the
// python sandbox, it's duplicated here so the sidecar stays a self-contained,
// dependency-free deployable.
//
// Trust boundary: the main server is the trusted decision layer; THIS process is
// the untrusted-execution half. It clones ONE repo (single-repo token), runs a
// bounded model-driven edit/verify loop, and pushes ONE fresh branch — it never
// merges, never pushes to the base branch, never force-pushes, and its network
// reach is confined by deployment (git host + the LLM proxy only).

// CodingRepo identifies the target repository (owner/name).
type CodingRepo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func (r CodingRepo) fullName() string { return r.Owner + "/" + r.Name }

// CodingSelector is a deterministic whole-repo relevance query authored by the
// model on the main server (lexical / treesitter / import_graph). Executed here
// to enumerate a finite candidate set instead of loading a huge repo.
type CodingSelector struct {
	Kind     string `json:"kind"`
	Query    string `json:"query"`
	Language string `json:"language,omitempty"`
}

// CodingLimits are the hard per-run bounds. Durations are nanoseconds (Go
// time.Duration marshals as int64 ns), matching codepr.CodingLimits.
type CodingLimits struct {
	Wall           int64 `json:"wall"`
	CPU            int64 `json:"cpu"`
	MemoryBytes    int64 `json:"memory_bytes"`
	PIDs           int   `json:"pids"`
	DiskBytes      int64 `json:"disk_bytes"`
	OutputBytes    int64 `json:"output_bytes"`
	CloneTimeout   int64 `json:"clone_timeout"`
	MaxVerifyIters int   `json:"max_verify_iters"`
	MaxCloneDepth  int   `json:"max_clone_depth"`
}

// CodingEgress is the default-deny host allowlist (advisory to this process;
// truly enforced at the network-namespace/proxy layer in deployment).
type CodingEgress struct {
	AllowHosts []string `json:"allow_hosts"`
}

// CodingJob is the /code-run request. Mirrors the main server's codingJobWire,
// PLUS the LLM proxy coordinates: the sidecar has no model of its own (the
// main server owns the model-agnostic LLM + its secrets), so the edit loop calls
// back to the main server over the internal network for each completion.
type CodingJob struct {
	ID         string     `json:"id"`
	Repo       CodingRepo `json:"repo"`
	CloneToken string     `json:"clone_token"`
	BaseBranch string     `json:"base_branch"`
	HeadBranch string     `json:"head_branch"`
	// ContinueBranch: HeadBranch already exists (a pull request this agent
	// opened earlier) and this run adds a commit to it, instead of starting a
	// fresh branch from BaseBranch. The push is still never forced, so if
	// anyone else pushed to it meanwhile the run is refused, not overwriting.
	ContinueBranch bool             `json:"continue_branch,omitempty"`
	Prompt         string           `json:"prompt"`
	SparsePaths    []string         `json:"sparse_paths,omitempty"`
	Selectors      []CodingSelector `json:"selectors,omitempty"`
	Limits         CodingLimits     `json:"limits"`
	Egress         CodingEgress     `json:"egress"`

	// LLMProxyURL + LLMProxyToken point at the main server's internal coding-LLM
	// endpoint (model-agnostic, metered). Empty ⇒ the runner cannot drive an
	// edit loop and returns StatusUnavailable, so a misconfiguration degrades
	// cleanly rather than doing half a job.
	LLMProxyURL   string `json:"llm_proxy_url"`
	LLMProxyToken string `json:"llm_proxy_token"`
}

// CodingVerifierResult is one quality gate's outcome, output already summarized.
type CodingVerifierResult struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
}

// CodingVerifierReport is the full gate outcome. HadTests distinguishes "tests
// passed" from "no tests to run" so the PR body can disclose it honestly.
type CodingVerifierReport struct {
	Ran       []CodingVerifierResult `json:"ran"`
	AllPassed bool                   `json:"all_passed"`
	HadTests  bool                   `json:"had_tests"`
}

// CodingDiffStat summarizes the produced change without inlining a huge diff.
type CodingDiffStat struct {
	Files        int  `json:"files"`
	Added        int  `json:"added"`
	Removed      int  `json:"removed"`
	PartialScope bool `json:"partial_scope"`
}

// CodingRunUsage is the metered resource cost of a run.
type CodingRunUsage struct {
	WallMS           int64 `json:"wall_ms"`
	CPUMS            int64 `json:"cpu_ms"`
	PeakMemBytes     int64 `json:"peak_mem_bytes"`
	VerifyIterations int   `json:"verify_iterations"`
}

// CodingResult is the /code-run response (JSON-compatible with
// codepr.CodingResult). Message is sanitized (no token, no host paths).
type CodingResult struct {
	Status        string               `json:"status"`
	HeadBranch    string               `json:"head_branch,omitempty"`
	DiffRef       string               `json:"diff_ref,omitempty"`
	Diff          string               `json:"diff,omitempty"`
	DiffStat      CodingDiffStat       `json:"diff_stat"`
	Verifier      CodingVerifierReport `json:"verifier"`
	Selectors     []CodingSelector     `json:"selectors,omitempty"`
	CandidateList []string             `json:"candidate_list,omitempty"`
	Usage         CodingRunUsage       `json:"usage"`
	Message       string               `json:"message,omitempty"`
}

// Coding status codes (JSON-compatible with codepr Status*).
const (
	CodingStatusOK          = "ok"
	CodingStatusNoGreen     = "no_green"
	CodingStatusTooLarge    = "too_large"
	CodingStatusBlocked     = "blocked"
	CodingStatusTimeout     = "timeout"
	CodingStatusError       = "error"
	CodingStatusUnavailable = "unavailable"
)

// Verifier kinds (JSON-compatible with codepr.VerifierKind).
const (
	verifierFmt   = "fmt"
	verifierLint  = "lint"
	verifierBuild = "build"
	verifierTest  = "test"
)
