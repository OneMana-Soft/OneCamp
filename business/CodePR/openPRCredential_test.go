package codepr

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A coding run's pull request is opened as the run's own identity, the one its
// branch was pushed with (credential.go), on every path that opens one: a
// green run and a draft for a red or timed-out one. It was opened with the
// workspace's GitHub connection, the admin's token across every organisation
// they belong to, so the pull request was theirs, on whatever repository a run
// allowed to work outside the linked ones reached.
func TestRun_OpensThePullRequestAsTheRunsOwnIdentity(t *testing.T) {
	for name, result := range map[string]CodingResult{
		"a green run":                 okResult(),
		"a red run, as a draft":       {Status: StatusNoGreen, HeadBranch: "onecamp-agent/fix-padding-abc"},
		"a timed-out run, as a draft": {Status: StatusTimeout, HeadBranch: "onecamp-agent/fix-padding-abc"},
	} {
		o, _ := baseOrch(&mockCodingRunner{Result: result})
		o.DraftOnRed = true
		var opened []Credential
		o.OpenPR = func(_ context.Context, cred Credential, _ RepoRef, _, _, _, _ string, _ bool) (string, error) {
			opened = append(opened, cred)
			return "https://github.com/o/r/pull/1", nil
		}
		if out := o.Run(context.Background(), task()); out.PRURL == "" {
			t.Fatalf("%s opened no pull request: %+v", name, out)
		}
		if len(opened) != 1 || opened[0].Token != "tok" || opened[0].Kind != CredentialUserConnector {
			t.Errorf("%s opened its pull request with %+v, want the run's own credential", name, opened)
		}
	}
}

// The live opener sends the run's token, and with no usable one it opens
// nothing: there is no other identity to fall back to.
func TestLiveOpenPRSendsTheRunsOwnToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	real := http.DefaultTransport
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Host+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"number": 3, "html_url": "https://github.com/o/r/pull/3"}`)), Request: r}, nil
	})
	defer func() { http.DefaultTransport = real }()

	repo := RepoRef{Owner: "o", Name: "r"}
	url, err := liveOpenPR(context.Background(), Credential{Token: "member-token", Kind: CredentialUserConnector}, repo, "onecamp-agent/x", "main", "Fix", "body", true)
	if err != nil || url != "https://github.com/o/r/pull/3" {
		t.Fatalf("opening the pull request: %q %v", url, err)
	}
	if len(seen) != 1 || seen[0] != "POST api.github.com/repos/o/r/pulls Bearer member-token" {
		t.Errorf("GitHub saw %v, want the one pull request, as the run's identity", seen)
	}

	// A credential the run may not use, a token without an identity
	// included, opens nothing.
	for _, cred := range []Credential{{}, {Token: "stale-token", Kind: CredentialNone}, {Kind: CredentialUserConnector}} {
		seen = nil
		if _, err := liveOpenPR(context.Background(), cred, repo, "onecamp-agent/x", "main", "Fix", "body", true); err == nil {
			t.Errorf("a run with the unusable credential %+v opened a pull request", cred)
		}
		if len(seen) != 0 {
			t.Errorf("a run with the unusable credential %+v reached GitHub: %v", cred, seen)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
