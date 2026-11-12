package business

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Qualifiers GitHub documents for searching issues and pull requests, and
// code.
var documentedQualifiers = []string{
	"repo", "org", "user", "owner", "is", "in", "type", "state", "reason", "author", "assignee",
	"mentions", "commenter", "involves", "team", "label", "milestone", "project", "status", "head",
	"base", "language", "comments", "interactions", "reactions", "draft", "review", "reviewed-by",
	"review-requested", "user-review-requested", "team-review-requested", "created", "updated",
	"closed", "merged", "archived", "no", "linked", "sort", "path", "size", "filename",
	"extension", "fork", "symbol", "content", "enterprise",
}

// A search is the person's words, in the one repository it was allowed:
// nothing they type is read by GitHub as a qualifier, an operator or a quote.
func TestRepoSearchQueryKeepsTheSearchInItsRepository(t *testing.T) {
	tries := map[string]string{
		"login bug":                       "repo:acme/web is:issue login bug",
		"  login\tbug  ":                  "repo:acme/web is:issue login bug",
		`login repo:someone/private`:      "repo:acme/web is:issue login",
		`login "repo:someone/private"`:    "repo:acme/web is:issue login",
		`"login repo:someone/private`:     "repo:acme/web is:issue login",
		`login NOT`:                       "repo:acme/web is:issue login",
		`NOT login`:                       "repo:acme/web is:issue login",
		`login not or and`:                "repo:acme/web is:issue login",
		`login OR (repo:someone/private)`: "repo:acme/web is:issue login",
		`login -repo:acme/web secret`:     "repo:acme/web is:issue login secret",
		`login\ repo:someone/private`:     "repo:acme/web is:issue login",
		`login+repo:someone/private`:      "repo:acme/web is:issue login",
		`login+NOT+x`:                     "repo:acme/web is:issue login x",
		`REPO:someone/private`:            "repo:acme/web is:issue",
		`created:>2020-01-01 crash`:       "repo:acme/web is:issue 2020-01-01 crash",
		`std::vector crash`:               "repo:acme/web is:issue crash",
		`"user:someone else" crash`:       "repo:acme/web is:issue else crash",
		`crash in-app_v2.0 #12 @ana it's`: "repo:acme/web is:issue crash in-app_v2.0 #12 @ana it's",
		`Ünïcode wörds`:                   "repo:acme/web is:issue Ünïcode wörds",
		"":                                "repo:acme/web is:issue",
		`(((`:                             "repo:acme/web is:issue",
	}
	for _, q := range documentedQualifiers {
		tries["login "+q+":someone"] = "repo:acme/web is:issue login"
		tries["login -"+q+":someone"] = "repo:acme/web is:issue login"
		tries[`login `+q+`:"someone else"`] = "repo:acme/web is:issue login someone else"
		tries[`login "`+q+`:someone"`] = "repo:acme/web is:issue login"
	}
	for terms, want := range tries {
		got, err := RepoSearchQuery(terms, "acme", "web", "is:issue")
		if err != nil || got != want {
			t.Errorf("searching %q: %q (%v), want %q", terms, got, err, want)
		}
	}
	// Only a repository's name is searched.
	for _, repo := range [][2]string{{"acme web", "x"}, {"acme", "web x"}, {"", "web"}, {"acme", ""}, {"acme", ".."},
		{"acme/x", "web"}, {"acme", "web repo:someone/private"}, {"Kcme", "web"}, {"-acme", "web"}} {
		if got, err := RepoSearchQuery("login", repo[0], repo[1]); err == nil {
			t.Errorf("searching %q/%q: %q, want it refused", repo[0], repo[1], got)
		}
	}
}

// A file is named inside the repository it was allowed: a dot segment, which a
// server resolves to another repository's file, is refused.
func TestRepoFilePathStaysInTheRepository(t *testing.T) {
	for path, want := range map[string]string{
		"src/app/main.go":     "src/app/main.go",
		"/src/app/main.go":    "src/app/main.go",
		"docs/a b#c?.md":      "docs/a%20b%23c%3F.md",
		".github/workflows/x": ".github/workflows/x",
		"a/..b/c..":           "a/..b/c..",
		"%2e%2e/x":            "%252e%252e/x",
		`..\x`:                `..%5Cx`,
	} {
		if got, err := repoFilePath(path); err != nil || got != want {
			t.Errorf("%q: %q (%v), want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "/", "..", "../x", "a/../../b", "./a", "a/./b", "a//b", "a/b/..", "src/",
		"../../../someone/private/contents/secret.txt"} {
		if got, err := repoFilePath(path); err == nil {
			t.Errorf("%q: %q, want it refused", path, got)
		}
	}
}

// On the public demo the workspace's GitHub connection writes nothing, a
// repository's webhook included; it reads, and revokes its token when GitHub
// is disconnected. Elsewhere it writes as asked.
func TestTheDemoWritesNothingToGitHub(t *testing.T) {
	var reached []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
	})
	send := func(method, address string) error {
		body := &closeCounter{Reader: strings.NewReader(`{"state":"closed"}`)}
		req, _ := http.NewRequest(method, address, body)
		resp, err := workspaceWrites{base: base}.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		} else if body.closed != 1 {
			t.Errorf("%s %s: refused with its body closed %d times, want once", method, address, body.closed)
		}
		return err
	}
	writes := [][2]string{
		{"PATCH", "https://api.github.com/repos/acme/web/issues/5"},
		{"POST", "https://api.github.com/repos/acme/web/issues/5/comments"},
		{"PATCH", "https://api.github.com/repos/acme/web/issues/comments/77"},
		{"DELETE", "https://api.github.com/repos/acme/web/issues/comments/77"},
		{"POST", "https://api.github.com/repos/acme/web/issues/5/labels"},
		{"DELETE", "https://api.github.com/repos/acme/web/issues/5/labels/bug"},
		{"POST", "https://api.github.com/repos/acme/web/git/refs"},
		{"POST", "https://api.github.com/repos/acme/web/pulls"},
		{"PUT", "https://api.github.com/repos/acme/web/contents/README.md"},
		{"POST", "https://api.github.com/user/repos"},
		{"POST", "https://api.github.com/graphql"},
		{"DELETE", "https://api.github.com/repos/acme/web"},
		{"POST", "https://API.GitHub.com/repos/acme/web/issues"},
		// A repository's webhook, registered, signed again or deleted.
		{"POST", "https://api.github.com/repos/acme/web/hooks"},
		{"PATCH", "https://api.github.com/repos/acme/web/hooks/9"},
		{"DELETE", "https://api.github.com/repos/acme/web/hooks/9"},
		{"PUT", "https://api.github.com/repos/acme/web/hooks/9"},
		{"POST", "https://api.github.com/repos/acme/web/hooks/9/pings"},
		// Shaped like revoking the token, and not it.
		{"POST", "https://api.github.com/applications/abc/token"},
		{"PATCH", "https://api.github.com/applications/abc/token"},
		{"DELETE", "https://api.github.com/applications/abc/grant"},
		{"DELETE", "https://api.github.com/applications/abc/token/x"},
		{"DELETE", "https://api.github.com/applications//token"},
	}
	reads := [][2]string{
		{"GET", "https://api.github.com/repos/acme/web/issues/5"},
		{"HEAD", "https://api.github.com/repos/acme/web"},
		{"GET", "https://api.github.com/search/issues?q=repo:acme/web"},
	}
	upkeep := [][2]string{
		// Disconnecting GitHub revokes the token: access taken away.
		{"DELETE", "https://api.github.com/applications/Iv1.abc123/token"},
		// Signing in to GitHub, and refreshing the token, aren't the API.
		{"POST", "https://github.com/login/oauth/access_token"},
	}

	t.Setenv("DEMO_MODE", "true")
	for _, w := range writes {
		reached = nil
		if err := send(w[0], w[1]); !errors.Is(err, ErrDemoGitHubWrite) || len(reached) != 0 {
			t.Errorf("on the demo, %s %s: %v (GitHub saw %v), want it refused", w[0], w[1], err, reached)
		}
	}
	for _, r := range append(append([][2]string{}, reads...), upkeep...) {
		reached = nil
		if err := send(r[0], r[1]); err != nil || len(reached) != 1 {
			t.Errorf("on the demo, %s %s: %v (GitHub saw %v), want it sent", r[0], r[1], err, reached)
		}
	}

	t.Setenv("DEMO_MODE", "")
	for _, w := range writes {
		reached = nil
		if err := send(w[0], w[1]); err != nil || len(reached) != 1 {
			t.Errorf("off the demo, %s %s: %v, want it sent", w[0], w[1], err)
		}
	}

	// Every client that carries the workspace's connection goes through it.
	if _, ok := githubHTTPClient.Transport.(workspaceWrites); !ok {
		t.Errorf("the shared GitHub client's transport is %T, want workspaceWrites", githubHTTPClient.Transport)
	}
}

// closeCounter counts how often a request's body is closed.
type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }
