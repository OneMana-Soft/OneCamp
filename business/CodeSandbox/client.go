package codesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// client.go — the HTTP Runner that talks to the isolated code-runner sidecar.
// It is the ONLY thing that reaches the runner; it authenticates with the shared
// token and bounds the call by the job's wall limit plus a grace window, so a
// hung sidecar can never hang the agent run.

// HTTPRunner submits jobs to a code-runner sidecar over its internal HTTP API.
type HTTPRunner struct {
	url    string
	token  string
	client *http.Client
}

// NewHTTPRunner builds a runner client for the configured endpoint + shared
// token. Returns nil when the URL is empty (sandbox not wired), so callers can
// pass the result straight to Run (a nil Runner degrades to "unavailable").
func NewHTTPRunner(url, token string) *HTTPRunner {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	return &HTTPRunner{
		url:   url,
		token: strings.TrimSpace(token),
		// No fixed client timeout: each Run derives its deadline from the job's
		// wall limit (below), which is always bounded.
		client: &http.Client{},
	}
}

// httpGrace is added to a job's wall limit for the transport deadline, so the
// sidecar has room to enforce + report its own timeout before the client gives
// up (avoiding a spurious transport error masking a real StatusTimeout).
const httpGrace = 20 * time.Second

// Run POSTs the job to the sidecar and decodes the Result. A transport/decode
// failure returns a non-nil error (Run() treats that as "unavailable"); a run
// that executed but failed comes back as a nil error with a non-OK status.
func (h *HTTPRunner) Run(ctx context.Context, job Job) (Result, error) {
	body, err := json.Marshal(job)
	if err != nil {
		return Result{}, err
	}

	wall := time.Duration(job.Limits.Wall)
	if wall <= 0 {
		wall = DefaultLimits().Wall
	}
	reqCtx, cancel := context.WithTimeout(ctx, wall+httpGrace)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Runner-Token", h.token)

	resp, err := h.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		// The sidecar's concurrency queue is full — a transient busy signal.
		return Result{}, fmt.Errorf("code-runner busy")
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("code-runner returned status %d", resp.StatusCode)
	}

	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return Result{}, err
	}
	return res, nil
}

// compile-time assertion.
var _ Runner = (*HTTPRunner)(nil)
