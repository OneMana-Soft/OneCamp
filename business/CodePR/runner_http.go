package codepr

// httpCodingRunner is the production CodingRunner: it hands one self-contained
// CodingJob to the isolated `code-runner` sidecar over the internal network and
// returns the parsed CodingResult. The main server never executes untrusted repo
// code itself — this is the trusted client half of the trust boundary; the
// sidecar (a separate, network-restricted deployable) is the untrusted half.
//
// Security notes:
//   - The clone token IS sent to the sidecar (it needs it to clone/push) but is
//     kept OUT of the public CodingJob's JSON (json:"-"): this client maps into
//     a private wire struct that includes it, so accidentally logging/auditing a
//     CodingJob can never leak the token. The token is also never logged here.
//   - Infra problems (busy/auth/unreachable/timeout) are mapped to a typed
//     CodingResult with a sanitized message and a nil error, so the orchestrator
//     surfaces them honestly (never a hang, never a raw transport error).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// HTTPRunnerConfig configures the sidecar client. BaseURL + Token come from admin
// config; Client is optional (a sane default with a generous timeout is used).
type HTTPRunnerConfig struct {
	BaseURL string
	Token   string
	Client  *http.Client
	// LLMProxyURL + LLMProxyToken are handed to the sidecar so its edit loop can
	// call back to the main server's model-agnostic LLM (the runner has no model
	// of its own). Empty ⇒ the sidecar reports unavailable rather than doing a
	// partial job.
	LLMProxyURL   string
	LLMProxyToken string
}

// runnerTokenHeader is the shared-secret header the sidecar requires (internal
// auth), matching the sandbox runner convention.
const runnerTokenHeader = "X-Runner-Token"

type httpCodingRunner struct {
	baseURL       string
	token         string
	client        *http.Client
	llmProxyURL   string
	llmProxyToken string
}

// NewHTTPCodingRunner builds a CodingRunner that dispatches to the sidecar. A nil
// or misconfigured base URL yields a runner that returns StatusUnavailable (so
// the feature degrades cleanly rather than panicking when enabled without a
// runner).
func NewHTTPCodingRunner(cfg HTTPRunnerConfig) CodingRunner {
	client := cfg.Client
	if client == nil {
		// Coding runs are long, so the transport ceiling is DERIVED from the same
		// timing source of truth as everything else (MaxRunWallClock) plus one
		// more dispatch overhead — never an independent literal that a raised
		// wall limit could silently outgrow. Ordering is deliberate: the
		// in-sandbox wall limit fires first, then the sidecar's own HTTP write
		// deadline, and only then this client — so a slow run is reported
		// honestly by the runner instead of being cut off here.
		client = &http.Client{Timeout: MaxRunWallClock() + CodingDispatchOverhead}
	}
	return &httpCodingRunner{
		baseURL:       strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		token:         strings.TrimSpace(cfg.Token),
		client:        client,
		llmProxyURL:   strings.TrimSpace(cfg.LLMProxyURL),
		llmProxyToken: strings.TrimSpace(cfg.LLMProxyToken),
	}
}

// codingJobWire is the on-the-wire request. It mirrors CodingJob but INCLUDES the
// clone token (sent to the trusted sidecar). Kept private so the token field
// only ever exists on the request path, never on a value that might be logged.
type codingJobWire struct {
	ID             string       `json:"id"`
	Repo           RepoRef      `json:"repo"`
	CloneToken     string       `json:"clone_token"`
	BaseBranch     string       `json:"base_branch"`
	HeadBranch     string       `json:"head_branch"`
	ContinueBranch bool         `json:"continue_branch,omitempty"`
	Prompt         string       `json:"prompt"`
	SparsePaths    []string     `json:"sparse_paths,omitempty"`
	Selectors      []Selector   `json:"selectors,omitempty"`
	Limits         CodingLimits `json:"limits"`
	Egress         EgressPolicy `json:"egress"`
	LLMProxyURL    string       `json:"llm_proxy_url,omitempty"`
	LLMProxyToken  string       `json:"llm_proxy_token,omitempty"`
}

// Run dispatches the job to the sidecar and returns the parsed result. Infra
// failures map to a typed, sanitized CodingResult (nil error) so the caller
// handles them via its normal non-ok path.
func (r *httpCodingRunner) Run(ctx context.Context, job CodingJob) (CodingResult, error) {
	if r.baseURL == "" {
		return CodingResult{Status: StatusUnavailable, Message: "The coding runner is not configured."}, nil
	}

	wire := codingJobWire{
		ID:             job.ID,
		Repo:           job.Repo,
		CloneToken:     job.CloneToken,
		BaseBranch:     job.BaseBranch,
		HeadBranch:     job.HeadBranch,
		ContinueBranch: job.ContinueBranch,
		Prompt:         job.Prompt,
		SparsePaths:    job.SparsePaths,
		Selectors:      job.Selectors,
		Limits:         job.Limits,
		Egress:         job.Egress,
		LLMProxyURL:    r.llmProxyURL,
		LLMProxyToken:  r.llmProxyToken,
	}
	buf, err := json.Marshal(wire)
	if err != nil {
		return CodingResult{Status: StatusError, Message: "Failed to prepare the coding job."}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/code-run", bytes.NewReader(buf))
	if err != nil {
		return CodingResult{Status: StatusError, Message: "Failed to build the coding request."}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set(runnerTokenHeader, r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		// Distinguish a deadline/cancel (timeout) from an unreachable runner.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return CodingResult{Status: StatusTimeout, Message: "The coding run exceeded its time limit."}, nil
		}
		return CodingResult{Status: StatusUnavailable, Message: "The coding runner is unavailable."}, nil
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
		var result CodingResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return CodingResult{Status: StatusError, Message: "The coding runner returned an unreadable response."}, nil
		}
		if strings.TrimSpace(result.Status) == "" {
			return CodingResult{Status: StatusError, Message: "The coding runner returned no status."}, nil
		}
		// Defensively sanitize everything the sidecar returned as free text. This
		// is the ONE seam where runner output enters the server, so scrubbing here
		// covers every downstream consumer at once.
		//
		// The verifier summaries matter as much as the message: they are raw build
		// and test output, they are rendered into the PR BODY and stored in the
		// code_pr_runs audit row, and the sidecar's own summarizer does no
		// redaction — so an absolute host path or a token-shaped string echoed by a
		// build step would otherwise be published to GitHub verbatim.
		result.Message = Sanitize(result.Message)
		result.Verifier = sanitizeReport(result.Verifier)
		result.Diff = Sanitize(result.Diff)
		return result, nil
	case resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusTooManyRequests:
		return CodingResult{Status: StatusUnavailable, Message: "The coding runner is busy. Try again shortly."}, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return CodingResult{Status: StatusUnavailable, Message: "The coding runner rejected the request (auth). Check its configuration."}, nil
	case resp.StatusCode == http.StatusGatewayTimeout:
		return CodingResult{Status: StatusTimeout, Message: "The coding run exceeded its time limit."}, nil
	default:
		snippet := Sanitize(string(raw))
		if len(snippet) > 200 {
			snippet = helpers.TruncateRunes(snippet, 200)
		}
		return CodingResult{Status: StatusError, Message: fmt.Sprintf("The coding runner failed (%d). %s", resp.StatusCode, snippet)}, nil
	}
}

// sanitizeReport scrubs host paths and token-shaped strings from every gate
// summary in a verifier report, leaving the structure (names, kinds, pass/fail,
// HadTests) untouched. Split out so it is directly unit-testable and so the
// scrubbing rule stays in one place. Pure.
func sanitizeReport(r VerifierReport) VerifierReport {
	for i := range r.Ran {
		r.Ran[i].Summary = Sanitize(r.Ran[i].Summary)
	}
	return r
}
