package business

// GitHub read: where a pull request stands, asked with a specific token.
//
// The coding agent continues its own pull request when someone follows up on
// it ("also handle the empty list"), instead of opening a second one beside
// the first. Before it pushes to that branch it confirms, with the identity
// that will push, that the pull request is still open and still points at the
// branch the agent created in the same repository. A merged or closed pull
// request, or one whose head is a fork, is never pushed to.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PullRequestHead is what the agent needs to know before pushing to a pull
// request's branch.
type PullRequestHead struct {
	Found    bool // false on 404/403: gone, or not visible to this token
	Open     bool // open and not merged
	HeadRef  string
	HeadRepo string // owner/name of the repository the head branch lives in
	BaseRef  string
	HTMLURL  string
}

// PullRequestHeadWithToken reads owner/name#number as token sees it. A 404/403
// is a definitive "not found", not an error.
func PullRequestHeadWithToken(ctx context.Context, token, owner, name string, number int) (PullRequestHead, error) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if owner == "" || name == "" || number <= 0 {
		return PullRequestHead{}, fmt.Errorf("owner, repo and a pull request number are required")
	}
	if strings.TrimSpace(token) == "" {
		return PullRequestHead{}, fmt.Errorf("a token is required")
	}
	return pullRequestHeadWithClient(ctx, tokenAuthClient(token), owner, name, number)
}

// pullRequestHeadWithClient issues the GET and interprets it. Split out so the
// parsing is tested against a mock transport.
func pullRequestHeadWithClient(ctx context.Context, client *http.Client, owner, name string, number int) (PullRequestHead, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, name, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PullRequestHead{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return PullRequestHead{}, fmt.Errorf("read pull request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized:
		return PullRequestHead{}, nil
	default:
		return PullRequestHead{}, fmt.Errorf("read pull request: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if jerr := json.Unmarshal(raw, &body); jerr != nil {
		return PullRequestHead{}, fmt.Errorf("read pull request: unreadable response")
	}
	out := PullRequestHead{
		Found:   true,
		Open:    strings.EqualFold(body.State, "open") && !body.Merged,
		HeadRef: strings.TrimSpace(body.Head.Ref),
		BaseRef: strings.TrimSpace(body.Base.Ref),
		HTMLURL: strings.TrimSpace(body.HTMLURL),
	}
	// A deleted fork leaves head.repo null: nothing to push to.
	if body.Head.Repo != nil {
		out.HeadRepo = strings.TrimSpace(body.Head.Repo.FullName)
	}
	return out, nil
}
