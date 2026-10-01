// DefaultFetchAttachment lives in the provider package (not the parent
// business package) so per-provider implementations can reuse it
// without creating a circular import on business/Import.
package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// defaultHTTPClient covers the typical attachment download path. Long
// timeout because some provider CDNs (Notion S3, Asana S3, Trello CDN)
// can take a while on multi-MB files.
var defaultHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

// DefaultFetchAttachment is the helper providers can call from inside
// their own FetchAttachment when no provider-specific auth is required.
// Returns ErrAttachmentGone for 404/410 and *ErrRateLimited for 429.
//
// Caller is responsible for calling RateLimiter().Wait first if it
// wants to be polite to the upstream.
func DefaultFetchAttachment(ctx context.Context, att SourceAttachment, dest io.Writer) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
	if err != nil {
		return "", 0, err
	}
	for k, v := range att.Headers {
		req.Header.Set(k, v)
	}
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return "", 0, ErrAttachmentGone
	case resp.StatusCode == http.StatusTooManyRequests:
		retryAfter := 5 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return "", 0, &ErrRateLimited{RetryAfter: retryAfter, Reason: "HTTP 429"}
	case resp.StatusCode >= 400:
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(dest, resp.Body)
	if err != nil {
		return "", 0, err
	}
	return resp.Header.Get("Content-Type"), n, nil
}
