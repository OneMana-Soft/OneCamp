package business

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// githubClient.go — per-user GitHub operations via the REST API. Uses the
// SSRF-safe HTTP client with the user's OAuth token. Distinct from the GitHub
// App integration (which is org-level for repo webhooks/checks).

const githubAPIBase = "https://api.github.com"

// GitHubTokenForUser returns the user's own GitHub access token, refreshing it
// when needed, or ("", nil) when that user has not connected GitHub.
//
// Exported because the credential a code-PR run should prefer is the REQUESTING
// USER's, not the workspace App's: GitHub then enforces the repository boundary
// itself, so a member cannot reach a repository they have no access to, and the
// resulting pull request is attributed to the person who actually asked for it.
// The connector's per-user isolation invariant still holds — the caller must pass
// the UUID of the principal the run executes as, never an arbitrary user.
func GitHubTokenForUser(ctx context.Context, userUUID uuid.UUID) (string, error) {
	return validAccessToken(ctx, userUUID, ProviderGitHub)
}

// githubToken is the in-package alias used by the connector's own tools.
func githubToken(ctx context.Context, userUUID uuid.UUID) (string, error) {
	return GitHubTokenForUser(ctx, userUUID)
}

// githubGET performs an authenticated GET and decodes JSON into out.
func githubGET(ctx context.Context, token, path string, out interface{}) error {
	endpoint := githubAPIBase + path
	if _, err := helpers.ValidateOutboundURL(endpoint, false); err != nil {
		return fmt.Errorf("github url rejected: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "OneCamp-Connector/1.0")

	client := helpers.SSRFSafeClient(false)
	client.Timeout = 15 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotConnected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github returned %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// githubPOST performs an authenticated POST with a JSON body.
func githubPOST(ctx context.Context, token, path string, payload interface{}, out interface{}) error {
	endpoint := githubAPIBase + path
	if _, err := helpers.ValidateOutboundURL(endpoint, false); err != nil {
		return fmt.Errorf("github url rejected: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Connector/1.0")

	client := helpers.SSRFSafeClient(false)
	client.Timeout = 15 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNotConnected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github returned %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// GitHubItem is a compact PR/issue view for the AI.
type GitHubItem struct {
	Title     string `json:"title"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updated_at"`
	Author    string `json:"author,omitempty"`
}

type githubSearchResponse struct {
	Items []struct {
		Title         string `json:"title"`
		Number        int    `json:"number"`
		State         string `json:"state"`
		HTMLURL       string `json:"html_url"`
		UpdatedAt     string `json:"updated_at"`
		RepositoryURL string `json:"repository_url"`
		User          struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"items"`
}

// GitHubListMyPRs returns open pull requests authored by or requesting review
// from the user. Read-only.
func GitHubListMyPRs(ctx context.Context, userUUID uuid.UUID, limit int) ([]GitHubItem, error) {
	token, err := githubToken(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNotConnected
	}
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	// Search PRs that are open and involve the authenticated user.
	q := url.QueryEscape("is:open is:pr involves:@me")
	path := fmt.Sprintf("/search/issues?q=%s&sort=updated&per_page=%d", q, limit)
	var sr githubSearchResponse
	if err := githubGET(ctx, token, path, &sr); err != nil {
		return nil, err
	}
	return mapGithubItems(sr), nil
}

// GitHubListMyIssues returns open issues assigned to the user. Read-only.
func GitHubListMyIssues(ctx context.Context, userUUID uuid.UUID, limit int) ([]GitHubItem, error) {
	token, err := githubToken(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNotConnected
	}
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	q := url.QueryEscape("is:open is:issue assignee:@me")
	path := fmt.Sprintf("/search/issues?q=%s&sort=updated&per_page=%d", q, limit)
	var sr githubSearchResponse
	if err := githubGET(ctx, token, path, &sr); err != nil {
		return nil, err
	}
	return mapGithubItems(sr), nil
}

// GitHubSearch runs a free-text issue/PR search scoped to things that involve
// the authenticated user, so it returns "your items matching X" without
// leaking unrelated public results. Generic + read-only; powers unified search.
func GitHubSearch(ctx context.Context, userUUID uuid.UUID, query string, limit int) ([]GitHubItem, error) {
	token, err := githubToken(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNotConnected
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	// Scope to the user's involvement so results stay personal + permission-safe
	// (GitHub only returns content the user's token can see anyway).
	q := url.QueryEscape(query + " involves:@me")
	path := fmt.Sprintf("/search/issues?q=%s&sort=updated&per_page=%d", q, limit)
	var sr githubSearchResponse
	if err := githubGET(ctx, token, path, &sr); err != nil {
		return nil, err
	}
	return mapGithubItems(sr), nil
}

// GitHubComment posts a comment on an issue or PR. WRITE — runs only after user
// confirmation. owner/repo/number identify the target.
func GitHubComment(ctx context.Context, userUUID uuid.UUID, owner, repo string, number int, body string) (string, error) {
	token, err := githubToken(ctx, userUUID)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNotConnected
	}
	if owner == "" || repo == "" || number <= 0 {
		return "", fmt.Errorf("owner, repo and number are required")
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	var resp struct {
		HTMLURL string `json:"html_url"`
	}
	if err := githubPOST(ctx, token, path, map[string]string{"body": body}, &resp); err != nil {
		return "", err
	}
	return resp.HTMLURL, nil
}

func mapGithubItems(sr githubSearchResponse) []GitHubItem {
	out := make([]GitHubItem, 0, len(sr.Items))
	for _, it := range sr.Items {
		repo := it.RepositoryURL
		// repository_url looks like https://api.github.com/repos/owner/name
		if idx := indexAfter(repo, "/repos/"); idx >= 0 {
			repo = repo[idx:]
		}
		out = append(out, GitHubItem{
			Title:     it.Title,
			Repo:      repo,
			Number:    it.Number,
			State:     it.State,
			URL:       it.HTMLURL,
			UpdatedAt: it.UpdatedAt,
			Author:    it.User.Login,
		})
	}
	return out
}

func indexAfter(s, sub string) int {
	i := -1
	for j := 0; j+len(sub) <= len(s); j++ {
		if s[j:j+len(sub)] == sub {
			i = j + len(sub)
			break
		}
	}
	return i
}
