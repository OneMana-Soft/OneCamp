package ai

// Transient-failure retry for provider HTTP calls.
//
// LLM providers routinely return HTTP 429 (rate limit) and 5xx (transient
// upstream) errors - especially on metered/free tiers like Groq, OpenAI
// tier-1, or a busy self-hosted server. Without retry, a single rate-limit
// blip fails the whole AI call. doHTTPWithRetry makes every provider call
// resilient: it retries 429/5xx (and connection errors) with bounded
// exponential backoff that HONORS the provider's Retry-After header, and it
// always respects context cancellation/deadline so a slow retry can never
// outlive the caller's budget.
//
// POST-to-completion is safe to retry: a 429/5xx response means the server did
// no work, so re-sending is idempotent from the caller's perspective.

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// retryConfig tunes the backoff. maxAttempts counts the first try.
type retryConfig struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
}

// defaultRetryConfig is the standard policy for synchronous calls. The max
// single wait is capped so honoring a large Retry-After can't blow the
// caller's timeout; bounded attempts keep latency predictable.
func defaultRetryConfig() retryConfig {
	return retryConfig{
		maxAttempts: 4,
		baseDelay:   500 * time.Millisecond,
		maxDelay:    20 * time.Second,
	}
}

// interactiveRetryConfig is the policy for user-facing calls (live chat, the
// agent loop). It HONORS the provider's Retry-After (the server's explicit
// "capacity is free in N seconds" signal) up to a bounded cap, so a brief rate
// limit on a metered tier recovers instead of erroring out - while staying far
// snappier than the background policy so a human never hangs for tens of
// seconds. Earlier this was a hard 2s fail-fast, which on a free-tier 429 gave
// up before the token window reopened and surfaced "rate limited" on every
// agent query; the long waits people saw before were the reasoning model
// thinking (now disabled), not this retry.
func interactiveRetryConfig() retryConfig {
	return retryConfig{
		maxAttempts: 3,
		baseDelay:   500 * time.Millisecond,
		maxDelay:    8 * time.Second,
	}
}

// retryConfigForOpts picks the patient or fail-fast policy based on whether a
// human is waiting on the request.
func retryConfigForOpts(opts ChatOptions) retryConfig {
	if opts.LowLatency {
		return interactiveRetryConfig()
	}
	return defaultRetryConfig()
}

// isRetryableStatus reports whether an HTTP status should be retried.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500 (often transient on cloud LLMs)
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// parseRetryAfter reads the Retry-After header (delta-seconds or HTTP-date).
// Returns 0 when absent/unparseable so the caller falls back to backoff.
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoffWait sleeps before the next attempt, preferring the server-suggested
// Retry-After when present, otherwise exponential backoff with jitter. Capped
// at cfg.maxDelay and cancellable via ctx.
func backoffWait(ctx context.Context, attempt int, cfg retryConfig, suggested time.Duration) error {
	wait := suggested
	if wait <= 0 {
		// Exponential backoff: base * 2^(attempt-1), plus up to 250ms jitter.
		backoff := cfg.baseDelay << (attempt - 1)
		wait = backoff + time.Duration(rand.Int63n(int64(250*time.Millisecond)))
	}
	if wait > cfg.maxDelay {
		wait = cfg.maxDelay
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// providerStatusError builds the error for a non-2xx provider response. A
// surviving 429 (rate limit that outlasted retry/backoff) is wrapped with
// ErrProviderRateLimited so callers can treat it as a neutral, non-outage
// event for the circuit breaker (see CircuitBreaker.RecordResult). label is
// the provider/endpoint prefix, e.g. "openai: API returned".
func providerStatusError(label string, code int, body []byte) error {
	if code == http.StatusTooManyRequests {
		return fmt.Errorf("%s status %d: %s: %w", label, code, string(body), ErrProviderRateLimited)
	}
	return fmt.Errorf("%s status %d: %s", label, code, string(body))
}

// doHTTPWithRetry sends a request built fresh on each attempt (newReq must
// rebuild it, since the body is consumed) and retries transient failures. The
// caller owns the returned response body. On exhaustion it returns the last
// response (so the caller can surface the real status/body) or the last error.
func doHTTPWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error), cfg retryConfig) (*http.Response, error) {
	if cfg.maxAttempts < 1 {
		cfg.maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= cfg.maxAttempts; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			// Network/transport error. Don't retry if the context is done.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt == cfg.maxAttempts {
				return nil, err
			}
			if werr := backoffWait(ctx, attempt, cfg, 0); werr != nil {
				return nil, err
			}
			continue
		}

		// Success or a non-retryable status, or out of attempts: hand back the
		// response for the caller to decode (including the final 429/5xx body).
		if !isRetryableStatus(resp.StatusCode) || attempt == cfg.maxAttempts {
			return resp, nil
		}

		// Retryable: honor Retry-After, drain+close, and try again.
		wait := parseRetryAfter(resp.Header)
		_ = resp.Body.Close()
		if werr := backoffWait(ctx, attempt, cfg, wait); werr != nil {
			return nil, fmt.Errorf("ai: provider rate-limited; cancelled while waiting to retry: %w", werr)
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("ai: request failed after %d attempts", cfg.maxAttempts)
}
