package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, c := range retryable {
		if !isRetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	notRetryable := []int{200, 201, 400, 401, 403, 404, 422}
	for _, c := range notRetryable {
		if isRetryableStatus(c) {
			t.Errorf("status %d should NOT be retryable", c)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	// delta-seconds
	h := http.Header{}
	h.Set("Retry-After", "5")
	if got := parseRetryAfter(h); got != 5*time.Second {
		t.Errorf("delta-seconds: got %v want 5s", got)
	}

	// absent
	if got := parseRetryAfter(http.Header{}); got != 0 {
		t.Errorf("absent: got %v want 0", got)
	}

	// negative -> 0
	h2 := http.Header{}
	h2.Set("Retry-After", "-3")
	if got := parseRetryAfter(h2); got != 0 {
		t.Errorf("negative: got %v want 0", got)
	}

	// unparseable -> 0
	h3 := http.Header{}
	h3.Set("Retry-After", "soon")
	if got := parseRetryAfter(h3); got != 0 {
		t.Errorf("unparseable: got %v want 0", got)
	}

	// HTTP-date in the future -> positive
	h4 := http.Header{}
	h4.Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
	if got := parseRetryAfter(h4); got <= 0 {
		t.Errorf("http-date: got %v want > 0", got)
	}
}

// doHTTPWithRetry should retry a 429 and then succeed, surfacing the 200.
func TestDoHTTPWithRetry_RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := retryConfig{maxAttempts: 4, baseDelay: time.Millisecond, maxDelay: 10 * time.Millisecond}
	resp, err := doHTTPWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, nil)
	}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("got %d calls want 3", got)
	}
}

// On persistent 429 it returns the final response (not an error) so the caller
// can surface the real status/body.
func TestDoHTTPWithRetry_ExhaustsAndReturnsLast(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	cfg := retryConfig{maxAttempts: 3, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := doHTTPWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, nil)
	}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("got status %d want 429", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != int32(cfg.maxAttempts) {
		t.Errorf("got %d calls want %d", got, cfg.maxAttempts)
	}
}

// A non-retryable status must NOT be retried.
func TestDoHTTPWithRetry_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := retryConfig{maxAttempts: 4, baseDelay: time.Millisecond, maxDelay: 5 * time.Millisecond}
	resp, err := doHTTPWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, nil)
	}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("got %d calls want 1 (no retry on 400)", got)
	}
}

// Context cancellation during backoff aborts promptly.
func TestDoHTTPWithRetry_RespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(60))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	cfg := retryConfig{maxAttempts: 4, baseDelay: time.Second, maxDelay: 20 * time.Second}
	start := time.Now()
	_, err := doHTTPWithRetry(ctx, srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	}, cfg)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took too long (%v); should abort on ctx cancel", time.Since(start))
	}
}

func TestFriendlyProviderError(t *testing.T) {
	if FriendlyProviderError(nil) != "" {
		t.Error("nil error should map to empty string")
	}
	// The reported Ollama failure must not leak the host and must be friendly.
	got := FriendlyProviderError(context.DeadlineExceeded)
	if got == "" || strings.Contains(got, "ollama") || strings.Contains(got, "11434") {
		t.Errorf("deadline mapping leaked internals or empty: %q", got)
	}
	raw := fmt.Errorf("ollama: stream request failed: Post \"http://ollama:11434/api/chat\": context deadline exceeded")
	if g := FriendlyProviderError(raw); strings.Contains(g, "11434") || strings.Contains(g, "http://") {
		t.Errorf("raw url leaked: %q", g)
	}
	if g := FriendlyProviderError(ErrProviderRateLimited); !strings.Contains(strings.ToLower(g), "rate") {
		t.Errorf("rate-limit mapping wrong: %q", g)
	}
}
