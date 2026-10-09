// DefaultFetchAttachment lives in the provider package (not the parent
// business package) so per-provider implementations can reuse it
// without creating a circular import on business/Import.
package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// downloadClient fetches every attachment an import brings in. An
// attachment's URL is whatever the source workspace holds, and anyone in it
// can add a link attachment pointing anywhere, so this is the SSRF-safe client
// (helpers.SSRFSafeClient): https only, and no address on this server or its
// networks, checked as each connection is made and at every redirect. Long
// timeout because some provider CDNs (Notion S3, Asana S3, Trello CDN) can take
// a while on multi-MB files.
var downloadClient = func() *http.Client {
	c := helpers.SSRFSafeClient(false)
	c.Timeout = 5 * time.Minute
	checkAddress := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := checkAddress(req, via); err != nil {
			return err
		}
		// A provider's credential was checked for the host it was set for, and
		// goes no further. net/http already drops it for a host that isn't
		// that one or under it; this drops it for any other host at all.
		if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return c
}()

// CredentialAllowed reports whether a download URL may be sent a provider's
// credential: it is https, and its host is one of hosts or a subdomain of
// one. The host is read from the parsed URL, never searched for in the text:
// "https://evil.example/?u=uploads.linear.app" is evil.example's, and so is
// "https://uploads.linear.app.evil.example/". The request is made from the
// same parse, so what is checked is what is dialled.
func CredentialAllowed(rawURL string, hosts ...string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false
	}
	for _, h := range hosts {
		h = strings.ToLower(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// WithAuthorization is att carrying an Authorization header of value when its
// URL may be sent the credential (CredentialAllowed for hosts), and carrying
// none when it may not.
func WithAuthorization(att SourceAttachment, value string, hosts ...string) SourceAttachment {
	headers := make(map[string]string, len(att.Headers)+1)
	for k, v := range att.Headers {
		if !strings.EqualFold(k, "Authorization") {
			headers[k] = v
		}
	}
	if CredentialAllowed(att.URL, hosts...) {
		headers["Authorization"] = value
	}
	att.Headers = headers
	return att
}

// DefaultFetchAttachment is the helper providers can call from inside
// their own FetchAttachment when no provider-specific auth is required.
// Returns ErrAttachmentGone for 404/410 and *ErrRateLimited for 429.
//
// Caller is responsible for calling RateLimiter().Wait first if it
// wants to be polite to the upstream.
func DefaultFetchAttachment(ctx context.Context, att SourceAttachment, dest io.Writer) (string, int64, error) {
	if _, err := helpers.ValidateOutboundURL(att.URL, false); err != nil {
		return "", 0, fmt.Errorf("attachment URL refused: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
	if err != nil {
		return "", 0, err
	}
	for k, v := range att.Headers {
		req.Header.Set(k, v)
	}
	resp, err := downloadClient.Do(req)
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
