package codepr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func runnerWith(status int, body string, capture *http.Request) CodingRunner {
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if capture != nil {
			b, _ := io.ReadAll(r.Body)
			*capture = *r
			capture.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return NewHTTPCodingRunner(HTTPRunnerConfig{BaseURL: "http://runner.internal", Token: "sekret", Client: client})
}

func sampleJob() CodingJob {
	return CodingJob{ID: "j1", Repo: RepoRef{Owner: "o", Name: "r"}, CloneToken: "ghp_secrettoken",
		BaseBranch: "main", HeadBranch: "onecamp-agent/fix-1", Prompt: "fix it", Limits: defaultCodingLimits()}
}

func TestHTTPRunner_SuccessParsesResult(t *testing.T) {
	body, _ := json.Marshal(okResult())
	var got http.Request
	r := runnerWith(http.StatusOK, string(body), &got)
	res, err := r.Run(context.Background(), sampleJob())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusOK || !res.Verifier.AllPassed {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Correct endpoint + auth header.
	if got.URL.Path != "/code-run" || got.Header.Get(runnerTokenHeader) != "sekret" {
		t.Fatalf("wrong request: path=%s token=%q", got.URL.Path, got.Header.Get(runnerTokenHeader))
	}
	// The clone token IS sent to the (trusted) sidecar on the wire.
	sent, _ := io.ReadAll(got.Body)
	if !strings.Contains(string(sent), "ghp_secrettoken") || !strings.Contains(string(sent), `"clone_token"`) {
		t.Fatalf("clone token must be sent to the sidecar: %s", string(sent))
	}
}

func TestHTTPRunner_TokenNotInPublicJob(t *testing.T) {
	// Guard the log-safety invariant: marshaling the PUBLIC CodingJob never
	// carries the token (json:"-"), even though the wire payload does.
	b, _ := json.Marshal(sampleJob())
	if strings.Contains(string(b), "ghp_secrettoken") {
		t.Fatalf("public CodingJob must not serialize the clone token: %s", string(b))
	}
}

func TestHTTPRunner_NotConfigured(t *testing.T) {
	r := NewHTTPCodingRunner(HTTPRunnerConfig{})
	res, _ := r.Run(context.Background(), sampleJob())
	if res.Status != StatusUnavailable {
		t.Fatalf("unconfigured runner should be unavailable, got %+v", res)
	}
}

func TestHTTPRunner_InfraStatusMappings(t *testing.T) {
	cases := []struct {
		http int
		want string
	}{
		{http.StatusServiceUnavailable, StatusUnavailable},
		{http.StatusTooManyRequests, StatusUnavailable},
		{http.StatusUnauthorized, StatusUnavailable},
		{http.StatusForbidden, StatusUnavailable},
		{http.StatusGatewayTimeout, StatusTimeout},
		{http.StatusInternalServerError, StatusError},
	}
	for _, c := range cases {
		r := runnerWith(c.http, `boom`, nil)
		res, err := r.Run(context.Background(), sampleJob())
		if err != nil {
			t.Fatalf("infra status should map to a typed result, not an error: %v", err)
		}
		if res.Status != c.want {
			t.Fatalf("HTTP %d → %s, want %s", c.http, res.Status, c.want)
		}
	}
}

func TestHTTPRunner_TransportErrorUnavailable(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	r := NewHTTPCodingRunner(HTTPRunnerConfig{BaseURL: "http://x", Client: client})
	res, err := r.Run(context.Background(), sampleJob())
	if err != nil || res.Status != StatusUnavailable {
		t.Fatalf("transport error should map to unavailable, got %+v err=%v", res, err)
	}
}

func TestHTTPRunner_BadJSON(t *testing.T) {
	r := runnerWith(http.StatusOK, `not json`, nil)
	res, _ := r.Run(context.Background(), sampleJob())
	if res.Status != StatusError {
		t.Fatalf("unreadable response should be an error status, got %+v", res)
	}
}

func TestHTTPRunner_SanitizesRunnerMessage(t *testing.T) {
	body, _ := json.Marshal(CodingResult{Status: StatusNoGreen, Message: "fail at /work/deadbeefcafe1234/src/x.go token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"})
	r := runnerWith(http.StatusOK, string(body), nil)
	res, _ := r.Run(context.Background(), sampleJob())
	if strings.Contains(res.Message, "ghp_") || strings.Contains(res.Message, "deadbeefcafe1234") {
		t.Fatalf("runner message must be sanitized: %q", res.Message)
	}
}

// TestSanitizeReport_ScrubsGateSummaries pins the leak this closes. Verifier
// summaries are RAW build and test output produced by the sidecar's own
// summarizer, which does no redaction — and they are rendered into the PR BODY
// (published to GitHub) and stored in the code_pr_runs audit row. Only
// result.Message used to be scrubbed, so a host path or a token-shaped string
// echoed by a build step travelled straight through.
func TestSanitizeReport_ScrubsGateSummaries(t *testing.T) {
	in := VerifierReport{
		HadTests:  true,
		AllPassed: false,
		Ran: []VerifierResult{
			{Name: "go build", Kind: VerifierBuild, Passed: false,
				Summary: "/work/coderun-9f2ab1c4d5e6/repo/main.go:12: undefined: foo"},
			{Name: "npm test", Kind: VerifierTest, Passed: false,
				Summary: "auth failed for token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		},
	}
	got := sanitizeReport(in)

	if strings.Contains(got.Ran[0].Summary, "coderun-9f2ab1c4d5e6") {
		t.Fatalf("the runner's filesystem layout must not reach the PR body; got %q", got.Ran[0].Summary)
	}
	if strings.Contains(got.Ran[1].Summary, "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
		t.Fatalf("a token-shaped string must never be published; got %q", got.Ran[1].Summary)
	}
	// The structure a reviewer relies on must survive untouched.
	if len(got.Ran) != 2 || got.Ran[0].Name != "go build" || got.Ran[0].Passed {
		t.Fatalf("scrubbing must not alter gate identity or outcome; got %+v", got.Ran)
	}
	if !got.HadTests || got.AllPassed {
		t.Fatalf("scrubbing must not alter report-level flags; got %+v", got)
	}
	// The useful signal must remain.
	if !strings.Contains(got.Ran[0].Summary, "undefined: foo") {
		t.Fatalf("the actionable part of the summary must survive; got %q", got.Ran[0].Summary)
	}
}
