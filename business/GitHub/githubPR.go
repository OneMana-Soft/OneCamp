package business

// GitHub write: opening a pull request. This is the single WRITE the main server
// performs directly for the code-PR agent (the coding runner pushes the branch;
// the server opens the PR from that branch). Reads (files, tree, PR diffs) reuse
// the existing helpers.
//
// Auth reuses the workspace's authenticated GitHub client (GitHubHTTPClient,
// auto-refreshed OAuth user-to-server token) — the same credential every other
// write in this package uses. It never force-pushes and never targets a base
// branch directly (the caller always supplies a fresh head branch). The request
// construction is split from the client fetch so it is unit-testable against a
// mock transport with no network.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// PullRequestResult carries the identifiers a caller needs after opening a PR.
type PullRequestResult struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
}

// ErrPullRequestExists indicates GitHub rejected the create because a PR already
// exists for the head→base pair (HTTP 422). The caller can treat this as
// non-fatal (surface the existing PR) rather than an error.
var ErrPullRequestExists = fmt.Errorf("a pull request already exists for this branch")

// CreatePullRequest opens a PR from head → base on owner/repo with the given
// title/body, optionally as a draft. Uses the workspace's authenticated GitHub
// client. Returns a typed result, or ErrPullRequestExists when one already
// exists, or a sanitized error otherwise.
func CreatePullRequest(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (*PullRequestResult, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("owner and repo are required")
	}
	if strings.TrimSpace(head) == "" || strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("head and base branches are required")
	}
	if strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(base)) {
		return nil, fmt.Errorf("head and base branches must differ")
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	return createPullRequestWithClient(ctx, client, owner, repo, head, base, title, body, draft)
}

// createPullRequestWithClient builds and sends the create-PR request using the
// provided client. Split out so tests can inject a mock transport. The GitHub
// API endpoint is POST /repos/{owner}/{repo}/pulls.
func createPullRequestWithClient(ctx context.Context, client *http.Client, owner, repo, head, base, title, body string, draft bool) (*PullRequestResult, error) {
	payload := map[string]interface{}{
		"title": strings.TrimSpace(title),
		"head":  head,
		"base":  base,
		"body":  body,
		"draft": draft,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode pull request: %w", err)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open pull request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusCreated:
		var pr struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			State   string `json:"state"`
			Draft   bool   `json:"draft"`
		}
		if err := json.Unmarshal(raw, &pr); err != nil {
			return nil, fmt.Errorf("decode pull request response: %w", err)
		}
		return &PullRequestResult{Number: pr.Number, HTMLURL: pr.HTMLURL, State: pr.State, Draft: pr.Draft}, nil
	case resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(string(raw)), "already exist"):
		return nil, ErrPullRequestExists
	default:
		// Surface a bounded, sanitized snippet — never the token (it's in the
		// Authorization header, not the body) but keep the message short.
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = helpers.TruncateRunes(snippet, 300)
		}
		return nil, fmt.Errorf("github create PR failed (%d): %s", resp.StatusCode, snippet)
	}
}
