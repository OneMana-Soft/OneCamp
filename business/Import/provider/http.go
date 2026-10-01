// Shared HTTP client tuned for provider API calls.
//
// Why a package-level singleton: every provider was previously calling
// http.DefaultClient.Do, which has NO timeout. A hung Asana/Jira/Notion
// upstream call would block a worker indefinitely, and the chunk
// reaper would only detect it after 5 minutes. This client caps the
// full request at 60s and gives the connection pool sane idle limits.
//
// Per-host concurrency is intentionally modest because the per-provider
// rate limiters already throttle outbound calls. Connection pooling
// just means we re-use TLS handshakes between calls.
package provider

import (
	"context"
	"net"
	"net/http"
	"time"
)

// SharedAPIClient is the HTTP client every provider should use for
// API metadata calls. Attachment downloads use a different client
// (DefaultFetchAttachment) because those have legitimately long bodies.
//
// Timeouts:
//   - Connect: 5s (TCP + TLS).
//   - TLS handshake: 5s.
//   - Response header: 30s — Asana/Jira can be slow on huge JQL
//     searches but anything above this almost certainly means a hung
//     upstream rather than a slow one.
//   - Whole request: 60s — caps total time per call so a stuck server
//     never blocks a worker beyond the chunk reaper window.
//
// Idle pool tuning:
//   - 32 idle conns total, 8 per host. Token-bucket rate limits keep
//     concurrency below this; the pool just amortises TLS handshakes.
var SharedAPIClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	},
}

// DoWithContext is a tiny convenience wrapper that always sets the
// request context before sending. Provider callers should use this so
// a cancelled job context interrupts in-flight HTTP calls cleanly.
func DoWithContext(ctx context.Context, req *http.Request) (*http.Response, error) {
	return SharedAPIClient.Do(req.WithContext(ctx))
}
