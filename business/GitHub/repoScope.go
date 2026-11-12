package business

// Where the workspace's GitHub connection may be pointed. That connection is
// the token of the admin who connected GitHub: "repo" scope on every
// organisation they belong to, far beyond the repositories this workspace
// links. So whatever a member's request puts into a call made with it is read
// here, in one place, and held to what was checked:
//
//   - an issue or pull request address names an owner and repository only in
//     the characters GitHub allows in names (ExtractGitHubIssueInfo), so the
//     name checked against a project's links is the name the API is asked for;
//   - a search sends GitHub the member's words with every qualifier, operator
//     and quote taken out (RepoSearchQuery): GitHub reads "repo:someone/x" in
//     them as one more repository to search, not as text;
//   - a file path has no dot segment (repoFilePath), which a server resolves to
//     a path outside the repository the read was allowed;
//   - and on the public demo nothing is written to GitHub at all
//     (workspaceWrites), whoever asks and however.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/akashc777/OneCamp/helpers"
)

// ownerName is a GitHub account's or organisation's name: letters, digits and
// hyphens, not starting with a hyphen; an enterprise's managed users add an
// underscore and the enterprise's short code.
var ownerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

// repoName is a repository's name: letters, digits, '.', '-' and '_', at most
// 100. ".", ".." and ".git" are not names (repoNamed).
var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// repoNamed reports whether owner/name is a name GitHub could give a
// repository. Pure.
func repoNamed(owner, name string) bool {
	return ownerName.MatchString(owner) && repoName.MatchString(name) &&
		name != "." && name != ".." && !strings.EqualFold(name, ".git")
}

// issueNumber is an issue's or pull request's number as its address writes
// it: digits alone, without a sign or a leading zero.
var issueNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

// ExtractGitHubIssueInfo reads the owner, repository and number of an issue or
// pull request from its address on GitHub, as GitHub writes it:
// http(s)://github.com/{owner}/{repo}/issues|pull/{number}, in any case for
// the scheme and host, www. allowed, and anything after the number (a
// "/files", a query, a fragment) ignored. A trailing ".git" on the repository
// is dropped. Anything else is refused: another host, a port or a user, a
// percent-encoded character, a dot segment anywhere in the path, a name
// GitHub couldn't give (repoNamed), a number that isn't one. Pure.
func ExtractGitHubIssueInfo(address string) (owner, repo string, number int, err error) {
	refused := fmt.Errorf("invalid GitHub issue/PR URL: %s", address)
	u, perr := url.Parse(strings.TrimSpace(address))
	if perr != nil || u.Opaque != "" || u.User != nil || u.RawPath != "" {
		return "", "", 0, refused
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if (scheme != "https" && scheme != "http") || (host != "github.com" && host != "www.github.com") {
		return "", "", 0, refused
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if n := len(segs); n > 4 && segs[n-1] == "" {
		segs = segs[:n-1] // a trailing slash
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", "", 0, refused
		}
	}
	if len(segs) < 4 || (segs[2] != "issues" && segs[2] != "pull") || !issueNumber.MatchString(segs[3]) {
		return "", "", 0, refused
	}
	owner, repo = segs[0], segs[1]
	if len(repo) > len(".git") && strings.EqualFold(repo[len(repo)-len(".git"):], ".git") {
		repo = repo[:len(repo)-len(".git")]
	}
	n, perr := strconv.ParseInt(segs[3], 10, 32)
	if perr != nil || !repoNamed(owner, repo) {
		return "", "", 0, refused
	}
	return owner, repo, int(n), nil
}

// errNotARepo refuses a search of something that isn't a repository's name.
var errNotARepo = errors.New("not a GitHub repository")

// RepoSearchQuery is the q of a GitHub search for terms in owner/name alone,
// with the search's own qualifiers (is:issue, ...). Of terms, only words are
// kept: every word with a colon (a qualifier, repo:, org:, user:, in:, is:,
// any key:value, negated or not) goes, and so do the operators AND, OR and
// NOT, and every quote, bracket and other character GitHub's search syntax
// reads. Two repo: qualifiers are either repository to GitHub, and a quote
// left open or a NOT before the scope undoes it, so the scope comes first and
// nothing a person typed can widen it. Pure.
func RepoSearchQuery(terms, owner, name string, qualifiers ...string) (string, error) {
	if !repoNamed(owner, name) {
		return "", errNotARepo
	}
	words := append([]string{"repo:" + owner + "/" + name}, qualifiers...)
	plain := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || strings.ContainsRune("._-/#@'*$&?!=,;:", r) {
			return r
		}
		return ' '
	}, terms)
	for _, word := range strings.Fields(plain) {
		if strings.ContainsRune(word, ':') {
			continue
		}
		switch strings.ToUpper(word) {
		case "AND", "OR", "NOT":
			continue
		}
		words = append(words, word)
	}
	return strings.Join(words, " "), nil
}

// errNotAFile refuses a path that names no file of the repository.
var errNotAFile = errors.New("that path doesn't name a file in the repository")

// repoFilePath is path as the contents API names a file of a repository: each
// segment escaped, the slashes between them kept, a leading slash dropped. A
// path with an empty, "." or ".." segment is refused: resolved, "../../../x/y"
// names a file of another repository. Pure.
func repoFilePath(path string) (string, error) {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", errNotAFile
		}
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/"), nil
}

// ErrDemoGitHubWrite is a write to GitHub refused on the public demo
// (workspaceWrites).
var ErrDemoGitHubWrite = errors.New(helpers.DemoGitHubMsg)

// workspaceWrites carries every request made with the workspace's GitHub
// connection (GitHubHTTPClient, githubHTTPClient). On the public demo, which
// everyone who opens it shares as one visitor and which holds a real
// connection, it refuses every request to GitHub's API that would change
// something there: a branch, a pull request, an issue's state, title, body,
// assignees or labels, a comment, a repository's webhook, whatever a later
// change adds. Reads go on, held to linked repositories as everywhere, and so
// does revoking the connection's token (demoRevokes). Elsewhere it changes
// nothing. base nil is the default transport, read at each request.
type workspaceWrites struct {
	base http.RoundTripper
}

func (g workspaceWrites) RoundTrip(r *http.Request) (*http.Response, error) {
	if helpers.DemoMode() && r.URL != nil && strings.EqualFold(r.URL.Hostname(), "api.github.com") &&
		r.Method != http.MethodGet && r.Method != http.MethodHead && !demoRevokes(r.Method, r.URL.Path) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, ErrDemoGitHubWrite
	}
	base := g.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// demoRevokes is the one write the demo still sends: revoking the connection's
// token, when an admin disconnects GitHub, which takes access away. Not even a
// repository's webhook is registered: the demo's GitHub story is fixtures
// (V-841), and the webhook was the write it made at every start, on a real
// repository (StartWebhookRefresh skips on the demo). Pure.
func demoRevokes(method, path string) bool {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return method == http.MethodDelete && len(segs) == 3 && segs[0] == "applications" && segs[1] != "" && segs[2] == "token"
}
