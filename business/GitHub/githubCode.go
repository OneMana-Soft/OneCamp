package business

// Read-only GitHub code retrieval for the code-aware issue/bug agent.
//
// The agent does NOT clone repos or run a sandbox. It analyses code via the
// GitHub REST API: fetch the files implicated by an issue (paths in a stack
// trace, code-search hits, or the repo tree), then hand the model the issue
// plus that real code so it can propose a root cause and a patch. This keeps
// the feature self-hosted-friendly (no disk, no checkout, no build runner) and
// reuses the existing authenticated client + token refresh.
//
// Everything here is read-only (GET). Writes (comments, branches, PRs) stay in
// the existing githubSync/githubBusiness paths and remain human-confirmed.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// maxCodeFileBytes bounds a single fetched file so one huge file can't blow the
// model's context budget (the caller also token-budgets the assembled prompt).
const maxCodeFileBytes = 64 * 1024

// CodeSearchHit is one result from a repo code search.
type CodeSearchHit struct {
	Path string `json:"path"`
	URL  string `json:"html_url"`
}

// GetDefaultBranch returns the repo's default branch (e.g. "main"), used as the
// ref for tree/content reads when the caller doesn't pin one.
func GetDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return "", err
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse repo info: %w", err)
	}
	if out.DefaultBranch == "" {
		return "main", nil
	}
	return out.DefaultBranch, nil
}

// FetchFileContent returns the decoded text of a file at path on ref (branch,
// tag, or sha). ref may be empty to use the repo default branch. Returns a
// bounded prefix for very large files. Binary or oversized blobs that the
// contents API returns without inline content yield an error the caller can
// skip over.
func FetchFileContent(ctx context.Context, owner, repo, path, ref string) (string, error) {
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return "", err
	}
	// Build /repos/{o}/{r}/contents/{path}?ref=...; escape each path segment
	// but keep the slashes between them.
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		url.PathEscape(owner), url.PathEscape(repo), escapePath(path))
	if strings.TrimSpace(ref) != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return "", err
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
		Size     int    `json:"size"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse contents for %s: %w", path, err)
	}
	if out.Type != "file" {
		return "", fmt.Errorf("%s is not a file", path)
	}
	if out.Encoding != "base64" || out.Content == "" {
		// Large files (>1MB) come back without inline content; skip them
		// rather than chasing the blob API in this read path.
		return "", fmt.Errorf("no inline content for %s (size %d)", path, out.Size)
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if derr != nil {
		return "", fmt.Errorf("decode %s: %w", path, derr)
	}
	text := string(decoded)
	if len(text) > maxCodeFileBytes {
		text = text[:maxCodeFileBytes] + "\n... [truncated]"
	}
	return text, nil
}

// SearchCode runs GitHub code search scoped to one repo, returning up to max
// file paths. Best-effort: code search is rate-limited (30/min) and only
// indexes the default branch of accessible repos, so callers treat an error or
// empty result as "no hits" and fall back to other retrieval signals.
func SearchCode(ctx context.Context, owner, repo, query string, max int) ([]CodeSearchHit, error) {
	if max <= 0 || max > 20 {
		max = 10
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("%s repo:%s/%s", strings.TrimSpace(query), owner, repo)
	u := fmt.Sprintf("https://api.github.com/search/code?q=%s&per_page=%d", url.QueryEscape(q), max)
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []CodeSearchHit `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse code search: %w", err)
	}
	if len(out.Items) > max {
		out.Items = out.Items[:max]
	}
	return out.Items, nil
}

// ListRepoTree returns blob (file) paths in the repo at ref plus whether the
// tree was truncated by GitHub (which happens for very large repos). ref may be
// empty (default branch). Callers use the paths as a retrieval-ranking signal,
// so a truncated/partial tree is still useful — they just must not assume it is
// complete.
func ListRepoTree(ctx context.Context, owner, repo, ref string, max int) (paths []string, truncated bool, err error) {
	if max <= 0 || max > 20000 {
		max = 12000
	}
	if strings.TrimSpace(ref) == "" {
		b, derr := GetDefaultBranch(ctx, owner, repo)
		if derr != nil {
			return nil, false, derr
		}
		ref = b
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, false, err
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, false, fmt.Errorf("parse repo tree: %w", err)
	}
	paths = make([]string, 0, len(out.Tree))
	for _, t := range out.Tree {
		if t.Type == "blob" {
			paths = append(paths, t.Path)
			if len(paths) >= max {
				break
			}
		}
	}
	return paths, out.Truncated, nil
}

// MergedPR is a merged pull request summarised for release-notes drafting.
type MergedPR struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	HTMLURL  string   `json:"html_url"`
	MergedAt string   `json:"merged_at"`
	Author   string   `json:"author"`
	Labels   []string `json:"labels"`
}

// ListMergedPullRequests returns PRs merged within the last sinceDays, newest
// first, capped at max. Read-only. Used to draft release notes / changelogs
// from real shipped work. Best-effort pagination (up to a few pages).
func ListMergedPullRequests(ctx context.Context, owner, repo string, sinceDays, max int) ([]MergedPR, error) {
	if sinceDays <= 0 {
		sinceDays = 14
	}
	if max <= 0 || max > 100 {
		max = 50
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -sinceDays)

	var out []MergedPR
	for page := 1; page <= 3 && len(out) < max; page++ {
		u := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=closed&sort=updated&direction=desc&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), page)
		body, gerr := githubGet(ctx, client, u)
		if gerr != nil {
			return nil, gerr
		}
		var raw []struct {
			Number   int        `json:"number"`
			Title    string     `json:"title"`
			Body     string     `json:"body"`
			HTMLURL  string     `json:"html_url"`
			MergedAt *time.Time `json:"merged_at"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if uerr := json.Unmarshal(body, &raw); uerr != nil {
			return nil, fmt.Errorf("parse pulls: %w", uerr)
		}
		if len(raw) == 0 {
			break
		}
		olderThanCutoff := 0
		for _, p := range raw {
			if p.MergedAt == nil {
				continue // closed-but-not-merged
			}
			if p.MergedAt.Before(cutoff) {
				olderThanCutoff++
				continue
			}
			labels := make([]string, 0, len(p.Labels))
			for _, l := range p.Labels {
				labels = append(labels, l.Name)
			}
			out = append(out, MergedPR{
				Number: p.Number, Title: p.Title, Body: p.Body, HTMLURL: p.HTMLURL,
				MergedAt: p.MergedAt.Format(time.RFC3339), Author: p.User.Login, Labels: labels,
			})
			if len(out) >= max {
				break
			}
		}
		// The list is sorted by updated desc; once a whole page is older than
		// the cutoff there's nothing newer to find.
		if olderThanCutoff == len(raw) {
			break
		}
	}
	return out, nil
}

// RecentCommit is one commit summarised for "what changed recently" answers.
type RecentCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"` // RFC3339 (committer date)
	HTMLURL string `json:"html_url"`
}

// ListRecentCommits returns commits on a branch, newest first, optionally
// bounded to the last sinceDays, capped at max. This is the DIRECT commits
// endpoint (GET /repos/{owner}/{repo}/commits) — NOT the search API.
//
// Why this exists as its own tool: GitHub's commit SEARCH endpoint is
// public-biased (it silently omits private repositories) and lags behind a
// search index, so an agent asked "what was committed today" against a private
// repo gets a false "no commits". The list endpoint reads the ref directly with
// the workspace's authenticated token, so it is real-time and works for private
// repos the token can access. branch may be empty to use the default branch.
func ListRecentCommits(ctx context.Context, owner, repo, branch string, sinceDays, max int) ([]RecentCommit, error) {
	if max <= 0 || max > 100 {
		max = 30
	}
	if strings.TrimSpace(branch) == "" {
		b, derr := GetDefaultBranch(ctx, owner, repo)
		if derr != nil {
			return nil, derr
		}
		branch = b
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits?sha=%s&per_page=%d",
		url.PathEscape(owner), url.PathEscape(repo), url.QueryEscape(branch), max)
	if sinceDays > 0 {
		since := time.Now().AddDate(0, 0, -sinceDays).UTC().Format(time.RFC3339)
		u += "&since=" + url.QueryEscape(since)
	}
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if uerr := json.Unmarshal(body, &raw); uerr != nil {
		return nil, fmt.Errorf("parse commits: %w", uerr)
	}
	out := make([]RecentCommit, 0, len(raw))
	for _, c := range raw {
		author := ""
		if c.Author != nil && c.Author.Login != "" {
			author = c.Author.Login
		} else {
			author = c.Commit.Author.Name
		}
		msg := c.Commit.Message
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i] // first line only
		}
		out = append(out, RecentCommit{
			SHA:     c.SHA,
			Message: strings.TrimSpace(msg),
			Author:  strings.TrimSpace(author),
			Date:    c.Commit.Committer.Date.Format(time.RFC3339),
			HTMLURL: c.HTMLURL,
		})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// escapePath escapes each segment of a repo-relative path while preserving the
// "/" separators, so "src/pkg/file.go" stays a valid contents-API path.
func escapePath(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// PRFile is one changed file in a pull request, with its unified-diff patch.
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"` // unified diff hunk; empty for binary/too-large files
}

// FetchPullRequestFiles returns up to max changed files of a PR (one page),
// with their diff patches. Returns truncated=true when the PR changed more
// files than max. Read-only. Best-effort: callers treat an error as "no diff".
func FetchPullRequestFiles(ctx context.Context, owner, repo string, number, max int) (files []PRFile, truncated bool, err error) {
	if max <= 0 || max > 100 {
		max = 50
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, false, err
	}
	// Ask for one page of up to 100; we bound to max below.
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/files?per_page=100",
		url.PathEscape(owner), url.PathEscape(repo), number)
	body, err := githubGet(ctx, client, u)
	if err != nil {
		return nil, false, err
	}
	var all []PRFile
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, false, fmt.Errorf("parse pr files: %w", err)
	}
	if len(all) > max {
		all = all[:max]
		truncated = true
	}
	// Bound each patch so one huge diff can't dominate the prompt.
	for i := range all {
		if len(all[i].Patch) > maxCodeFileBytes {
			all[i].Patch = all[i].Patch[:maxCodeFileBytes] + "\n... [diff truncated]"
		}
	}
	return all, truncated, nil
}
