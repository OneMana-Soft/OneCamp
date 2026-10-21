// Package slack is a Slack Web API client, one for everything OneCamp asks of
// Slack: the live bridge (business/SlackBridge) and the import's API reader
// (business/SlackImport). Every call is a form POST with the token as a
// bearer, which every Slack method accepts; a refusal ({"ok": false}) comes
// back as *Error; and a 429 is waited out by its Retry-After, capped, as
// often as the caller allows.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Base is Slack's API address: a var so tests can point it at a fake server.
var Base = "https://slack.com/api/"

// httpClient is what calls go through; its timeout is per request.
var httpClient = &http.Client{}

// Error is a Slack refusal ({"ok": false, "error": code}), with the scope it
// needed when it was missing_scope.
type Error struct {
	Method string
	Code   string
	Needed string
}

func (e *Error) Error() string { return "slack " + e.Method + ": " + e.Code }

// Reply is what a method answers: whether it was ok, and the code it refused
// with. OK is one; a reply type embeds it.
type Reply interface {
	refusal() (ok bool, code, needed string)
}

// OK is what every Slack reply carries, with the cursor of a paged one.
type OK struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Needed string `json:"needed"`
	Meta   struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (o OK) refusal() (bool, string, string) { return o.OK, o.Error, o.Needed }

// Next is a paged reply's cursor for its next page, "" on the last.
func (o OK) Next() string { return o.Meta.NextCursor }

// Client calls Slack with one token.
type Client struct {
	Token string
	// Retries is how many times a rate-limited call is tried again: the
	// bridge's listeners once, a reader paging a workspace more.
	Retries int
	// MaxWait caps one Retry-After wait (30 s when zero).
	MaxWait time.Duration
	// Timeout bounds one request (10 s when zero).
	Timeout time.Duration
}

// Call POSTs form to method and decodes the reply into out. A refusal is
// *Error; a 429 is tried again after its Retry-After, Retries times.
func (c Client) Call(ctx context.Context, method string, form url.Values, out Reply) error {
	maxWait := c.MaxWait
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for attempt := 0; ; attempt++ {
		body, status, retryAfter, err := c.post(ctx, method, form, timeout)
		if err != nil {
			return err
		}
		if status == http.StatusTooManyRequests {
			if attempt >= c.Retries {
				return &Error{Method: method, Code: "ratelimited"}
			}
			wait := min(max(retryAfter, time.Second), maxWait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("slack %s: http %d", method, status)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if ok, code, needed := out.refusal(); !ok {
			if code == "" {
				code = "unknown_error"
			}
			return &Error{Method: method, Code: code, Needed: needed}
		}
		return nil
	}
}

// post is one request: its body, status and Retry-After.
func (c Client) post(ctx context.Context, method string, form url.Values, timeout time.Duration) ([]byte, int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Base+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("slack %s: %w", method, err)
	}
	var retryAfter time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	return body, resp.StatusCode, retryAfter, nil
}

// A file's private link opens only with a token: Download fetches one.

// ErrNotSlackFile is a link that isn't Slack's own. A file shared from another
// service links there, and the token is never sent to it.
var ErrNotSlackFile = errors.New("not a Slack file link")

// ErrFileGone is a file Slack no longer has.
var ErrFileGone = errors.New("Slack no longer has the file")

// ErrFileTooLarge is a file over the caller's limit.
var ErrFileTooLarge = errors.New("the file is over the limit")

// downloadTimeout bounds one file's download.
const downloadTimeout = 10 * time.Minute

// downloadClient follows a redirect only to Slack's own hosts, where the
// client drops the token for any host but the first anyway.
var downloadClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || !slackHost(req.URL) {
		return fmt.Errorf("slack file: redirected away from Slack")
	}
	return nil
}}

// atBase is whether u is the host Base points at: Slack's API, a test's fake
// Slack, or a mirror an operator runs.
func atBase(u *url.URL) bool {
	b, err := url.Parse(Base)
	return err == nil && u.Scheme == b.Scheme && strings.EqualFold(u.Host, b.Host)
}

// slackHost is whether u is one of Slack's hosts over https, or Base's.
func slackHost(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return atBase(u) || u.Scheme == "https" && (h == "slack.com" || strings.HasSuffix(h, ".slack.com") ||
		strings.HasSuffix(h, ".slack-edge.com") || strings.HasSuffix(h, ".slack-files.com"))
}

// fileLink is whether link may carry the token: Slack's file host, or Base's.
func fileLink(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return false
	}
	return atBase(u) || u.Scheme == "https" && strings.EqualFold(u.Hostname(), "files.slack.com")
}

// Download writes the file at link to w, at most max bytes, and answers how
// many it wrote and the type Slack served them as. The token goes only to
// Slack's file host (ErrNotSlackFile); a 429 is waited out as Call waits one
// out. A file over max is ErrFileTooLarge, with what was written of it left
// for the caller to throw away.
func (c Client) Download(ctx context.Context, link string, w io.Writer, max int64) (int64, string, error) {
	if !fileLink(link) {
		return 0, "", ErrNotSlackFile
	}
	maxWait := c.MaxWait
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	for attempt := 0; ; attempt++ {
		n, kind, retryAfter, err := c.get(ctx, link, w, max)
		if retryAfter == 0 {
			return n, kind, err
		}
		if attempt >= c.Retries {
			return 0, "", &Error{Method: "file", Code: "ratelimited"}
		}
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-time.After(min(retryAfter, maxWait)):
		}
	}
}

// get is one download; a 429 answers its Retry-After (a second at least) and
// writes nothing.
func (c Client) get(ctx context.Context, link string, w io.Writer, max int64) (int64, string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return 0, "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := downloadClient.Do(req)
	if err != nil {
		return 0, "", 0, fmt.Errorf("slack file: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 1 {
			wait = time.Duration(s) * time.Second
		}
		return 0, "", wait, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return 0, "", 0, ErrFileGone
	case resp.StatusCode != http.StatusOK:
		return 0, "", 0, fmt.Errorf("slack file: http %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, "", 0, fmt.Errorf("slack file: %w", err)
	}
	if n > max {
		return n, "", 0, ErrFileTooLarge
	}
	return n, resp.Header.Get("Content-Type"), 0, nil
}
