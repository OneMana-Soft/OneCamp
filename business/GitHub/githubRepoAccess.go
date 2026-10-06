package business

// GitHub read: verifying the connected account can actually reach a repository.
// This backs the code-PR agent's "work on any repo I have access to" UX — the
// governed path must never be MORE restricted than the raw GitHub MCP tools, so
// before it accepts an explicitly-named repo that isn't in the linked set it
// confirms the workspace's authenticated GitHub credential can see it (a cheap
// GET /repos/{owner}/{name}). This fails FAST and HONESTLY: an inaccessible or
// misspelled repo becomes a clear "I can't reach owner/name" instead of a stall.
//
// Auth reuses the same auto-refreshed client every other GitHub call uses. The
// request build is split from the client fetch so it is unit-testable against a
// mock transport with no network.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RepoAccess is what one identity may actually DO with a repository. Visibility
// and writability are different questions and the distinction matters: a coding
// run needs to push a branch and open a pull request, so "I can see it" is not
// enough. Checking only visibility meant a read-only collaborator passed the gate
// and the run then cloned the repository, called the model for its whole budget,
// ran the build and the test suite, and failed at the very last step on a 403.
type RepoAccess struct {
	Visible       bool
	CanPush       bool
	Archived      bool   // archived repositories reject every write, for everyone
	DefaultBranch string // useful when the task named no base branch
}

// Writable reports whether a coding run could actually land a branch here.
func (a RepoAccess) Writable() bool { return a.Visible && a.CanPush && !a.Archived }

// RepoPushAccessWithToken reports what a SPECIFIC token may do with owner/name.
// One GET /repos/{owner}/{name} answers all of it: GitHub returns a `permissions`
// object for an authenticated request, plus the archived flag and the default
// branch, so the write check costs no extra round trip over the visibility check
// it replaces.
//
// A 404/403 is a definitive "not visible", not an error, so the caller can tell
// "checked, no access" from "couldn't check". The token is sent in the
// Authorization header only.
func RepoPushAccessWithToken(ctx context.Context, token, owner, name string) (RepoAccess, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return RepoAccess{}, fmt.Errorf("owner and repo are required")
	}
	if strings.TrimSpace(token) == "" {
		return RepoAccess{}, fmt.Errorf("a token is required")
	}
	return repoPushAccessWithClient(ctx, tokenAuthClient(token), owner, name)
}

// repoPushAccessWithClient issues the GET and interprets the response. Split from
// the public entry point so the permission parsing — the part that decides whether
// a run is allowed to push at all — is unit-tested against a mock transport with
// no network.
func repoPushAccessWithClient(ctx context.Context, client *http.Client, owner, name string) (RepoAccess, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RepoAccess{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return RepoAccess{}, fmt.Errorf("verify repo access: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized:
		return RepoAccess{Visible: false}, nil
	default:
		return RepoAccess{}, fmt.Errorf("verify repo access: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Archived      bool   `json:"archived"`
		Disabled      bool   `json:"disabled"`
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Admin    bool `json:"admin"`
			Maintain bool `json:"maintain"`
			Push     bool `json:"push"`
		} `json:"permissions"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if jerr := json.Unmarshal(raw, &body); jerr != nil {
		return RepoAccess{}, fmt.Errorf("verify repo access: unreadable response")
	}
	return RepoAccess{
		Visible: true,
		// admin and maintain both imply push; GitHub sets push for them anyway,
		// but treating them as sufficient is explicit rather than relying on that.
		CanPush:       body.Permissions.Push || body.Permissions.Maintain || body.Permissions.Admin,
		Archived:      body.Archived || body.Disabled,
		DefaultBranch: strings.TrimSpace(body.DefaultBranch),
	}, nil
}

// tokenAuthClient returns an HTTP client that attaches a bearer token to every
// request. Kept tiny and local so the token lives only in the transport.
func tokenAuthClient(token string) *http.Client {
	return &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrippers must not modify the caller's request.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// VerifyRepoAccess reports whether the workspace's connected GitHub account can
// access owner/name. It returns:
//   - (true, nil)  when GitHub answers 200 (the repo exists and is visible);
//   - (false, nil) when GitHub answers 404 or 403 (no such repo / no access) —
//     a definitive "can't use it", not an error;
//   - (false, err) only on a transport/auth/unexpected-status failure, so the
//     caller can distinguish "checked, no access" from "couldn't check".
//
// It never follows a repo's contents or leaks the token (the credential lives in
// the client's Authorization header, never in the URL or a returned message).
func VerifyRepoAccess(ctx context.Context, owner, name string) (bool, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return false, fmt.Errorf("owner and repo are required")
	}
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return false, err
	}
	return verifyRepoAccessWithClient(ctx, client, owner, name)
}

// VerifyRepoAccessWithToken reports whether a SPECIFIC token can reach
// owner/name, using the identical request and status mapping as VerifyRepoAccess.
//
// This exists so a code-PR run can check access with the CREDENTIAL IT WILL
// ACTUALLY PUSH WITH. Checking with the workspace credential and then pushing
// with a user's (or the reverse) is how a run gets cleared against one identity's
// permissions and executed under another's — the check has to be bound to the
// same token or it proves nothing.
//
// The token is sent in the Authorization header only; it never enters the URL or
// any returned message.
func VerifyRepoAccessWithToken(ctx context.Context, token, owner, name string) (bool, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return false, fmt.Errorf("owner and repo are required")
	}
	if strings.TrimSpace(token) == "" {
		return false, fmt.Errorf("a token is required")
	}
	return verifyRepoAccessWithClient(ctx, tokenAuthClient(token), owner, name)
}

// verifyRepoAccessWithClient issues the GET /repos/{owner}/{name} using the
// provided client. Split out so tests can inject a mock transport.
func verifyRepoAccessWithClient(ctx context.Context, client *http.Client, owner, name string) (bool, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("verify repo access: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized:
		// Definitive: the account can't see this repo (missing, private-to-others,
		// or the token lacks scope). Not an error — a clear "no".
		return false, nil
	default:
		return false, fmt.Errorf("verify repo access: unexpected status %d", resp.StatusCode)
	}
}
