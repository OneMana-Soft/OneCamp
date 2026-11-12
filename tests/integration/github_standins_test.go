//go:build integration

package integration_test

// GitHub's stand-ins on this edition: the AI edition keeps fakeGitHub and
// refuseGitHub in its agent_requester_test.go, which this edition doesn't
// have, and the shared data_move_quiet_test.go calls them. The code is the
// AI edition's as it is.

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub answers GitHub's REST API from canned bodies, by path, until the
// test ends; every other request goes out as usual. The GitHub sync and import
// call through the default transport, so that is where it stands in. A path it
// has no body for is a 404, and a list past its first page is empty.
func fakeGitHub(t *testing.T, bodies map[string]string) {
	t.Helper()
	real := http.DefaultTransport
	http.DefaultTransport = githubStandIn{real: real, bodies: bodies}
	t.Cleanup(func() { http.DefaultTransport = real })
}

type githubStandIn struct {
	real   http.RoundTripper
	bodies map[string]string
}

func (g githubStandIn) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return g.real.RoundTrip(r)
	}
	status, body := http.StatusOK, "[]"
	if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page <= 1 {
		var ok bool
		if body, ok = g.bodies[r.URL.Path]; !ok {
			status, body = http.StatusNotFound, `{"message":"Not Found"}`
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// refuseGitHub keeps a test off GitHub itself: until it ends, a request to a
// GitHub host that reaches the network is refused, and the test fails naming
// it. Called first, beneath a fakeGitHub, it proves the stand-in answered all
// of GitHub's API (api.github.com never gets past one) from the test's start.
func refuseGitHub(t *testing.T) {
	t.Helper()
	real := http.DefaultTransport
	g := &githubRefusal{real: real}
	http.DefaultTransport = g
	t.Cleanup(func() {
		http.DefaultTransport = real
		g.mu.Lock()
		defer g.mu.Unlock()
		if len(g.reached) > 0 {
			t.Errorf("the test reached GitHub itself, past its stand-in: %v", g.reached)
		}
	})
}

type githubRefusal struct {
	real    http.RoundTripper
	mu      sync.Mutex
	reached []string
}

func (g *githubRefusal) RoundTrip(r *http.Request) (*http.Response, error) {
	if h := r.URL.Hostname(); h == "github.com" || strings.HasSuffix(h, ".github.com") || strings.HasSuffix(h, ".githubusercontent.com") {
		g.mu.Lock()
		g.reached = append(g.reached, r.Method+" "+r.URL.String())
		g.mu.Unlock()
		return nil, fmt.Errorf("refused %s: this test's GitHub is its stand-in", r.URL)
	}
	return g.real.RoundTrip(r)
}
