package basecamp

// Talking to Basecamp's bc3 API (github.com/basecamp/bc3-api, read 19 Oct
// 2026): https only, OAuth 2 bearer tokens, a User-Agent with the app's name
// and a contact (a request without one is refused), about 50 requests per 10
// seconds per IP (429 with Retry-After), and pages followed through the Link
// header's rel="next" (15, 30, 50, then 100 a page). Routes are the flat ones
// (/todolists/3/todos.json), which the API now calls canonical; the
// /buckets/{project}/ forms are legacy.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
)

// launchpadBase is 37signals' sign-in service; a var so tests can point it at
// a fake.
var launchpadBase = "https://launchpad.37signals.com"

// apiOrigins are the only places the token is sent: the sign-in service, and
// the API ("All URLs start with https://3.basecampapi.com/999999999/. URLs are
// HTTPS only.", bc3-api README at 8e811073fd, 30 Sep 2026). Basecamp's
// answers name the addresses read next (an account's API base, a next page, a
// project's dock), and each is read only on one of these.
var apiOrigins = []string{"https://launchpad.37signals.com", "https://3.basecampapi.com"}

// sameOrigin reports whether two addresses share a scheme and host:port,
// ignoring case. Addresses that don't parse never match.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(b)
	return err == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// onAPI reports whether address is on one of apiOrigins: the same scheme,
// host and port, exactly.
func onAPI(address string) bool {
	for _, o := range apiOrigins {
		if sameOrigin(address, o) {
			return true
		}
	}
	return false
}

const userAgent = "OneCamp importer (https://onemana.dev)"

// maxPages bounds a listing so a runaway one can't hang a worker: past the
// first pages Basecamp gives 100 a page, so about 20,000 items.
const maxPages = 200

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// get reads one page of url into out and returns the next page's URL, "" at
// the last.
func (p *Provider) get(ctx context.Context, tok, url string, out any) (string, error) {
	if !onAPI(url) {
		return "", fmt.Errorf("basecamp: an address off Basecamp's API (%s) isn't read", helpers.WithoutURLQueries(url))
	}
	if err := p.rl.Wait(ctx); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := importProvider.SharedAPIClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := 10 * time.Second
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
		return "", &importProvider.ErrRateLimited{RetryAfter: wait, Reason: "Basecamp 429"}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return "", &importProvider.TokenRejected{Msg: fmt.Sprintf("Basecamp refused the sign-in (HTTP %d); reconnect Basecamp", resp.StatusCode)}
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("basecamp HTTP %d: %s", resp.StatusCode, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("basecamp %s: %w", req.URL.Path, err)
	}
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		return m[1], nil
	}
	return "", nil
}

// getAll reads every page of a listing into out, a pointer to a slice.
func (p *Provider) getAll(ctx context.Context, tok, url string, out any) error {
	var all []json.RawMessage
	for page := 0; url != "" && page < maxPages; page++ {
		var items []json.RawMessage
		next, err := p.get(ctx, tok, url, &items)
		if err != nil {
			return err
		}
		all = append(all, items...)
		url = next
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
