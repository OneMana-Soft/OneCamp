package business

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRepoPushAccessWithClient covers the distinction the coding agent depends on:
// a 200 from GET /repos proves the identity can SEE the repository, not that it can
// push to it. Reading only the status code meant a read-only collaborator was
// cleared to run, and the run then cloned, called the model for its whole budget,
// ran the build and tests, and failed at the push.
func TestRepoPushAccessWithClient(t *testing.T) {
	const pushBody = `{"archived":false,"default_branch":"main","permissions":{"admin":false,"maintain":false,"push":true,"pull":true}}`
	const readBody = `{"archived":false,"default_branch":"main","permissions":{"admin":false,"maintain":false,"push":false,"pull":true}}`
	const adminBody = `{"archived":false,"default_branch":"trunk","permissions":{"admin":true,"push":false,"pull":true}}`
	const maintainBody = `{"archived":false,"default_branch":"main","permissions":{"maintain":true,"push":false,"pull":true}}`
	const archivedBody = `{"archived":true,"default_branch":"main","permissions":{"push":true,"pull":true}}`
	const disabledBody = `{"disabled":true,"default_branch":"main","permissions":{"push":true,"pull":true}}`

	cases := map[string]struct {
		status       int
		body         string
		wantVisible  bool
		wantPush     bool
		wantArchived bool
		wantWritable bool
		wantBranch   string
	}{
		"write access":           {200, pushBody, true, true, false, true, "main"},
		"read only":              {200, readBody, true, false, false, false, "main"},
		"admin implies push":     {200, adminBody, true, true, false, true, "trunk"},
		"maintain implies push":  {200, maintainBody, true, true, false, true, "main"},
		"archived blocks writes": {200, archivedBody, true, true, true, false, "main"},
		"disabled blocks writes": {200, disabledBody, true, true, true, false, "main"},
		"not found":              {404, `{}`, false, false, false, false, ""},
		"forbidden":              {403, `{}`, false, false, false, false, ""},
	}
	for name, tc := range cases {
		got, err := repoPushAccessWithClient(context.Background(), mockClient(tc.status, tc.body, nil), "acme", "svc")
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if got.Visible != tc.wantVisible || got.CanPush != tc.wantPush || got.Archived != tc.wantArchived {
			t.Errorf("%s: got %+v, want visible=%v push=%v archived=%v",
				name, got, tc.wantVisible, tc.wantPush, tc.wantArchived)
		}
		if got.Writable() != tc.wantWritable {
			t.Errorf("%s: Writable()=%v want %v (%+v)", name, got.Writable(), tc.wantWritable, got)
		}
		if got.DefaultBranch != tc.wantBranch {
			t.Errorf("%s: default branch %q want %q", name, got.DefaultBranch, tc.wantBranch)
		}
	}
}

// An unexpected status is an error, not a denial, so the caller can tell "checked,
// not allowed" from "couldn't check" — and refuse rather than assume either way.
func TestRepoPushAccessWithClient_UnexpectedStatusIsAnError(t *testing.T) {
	if _, err := repoPushAccessWithClient(context.Background(), mockClient(500, `{}`, nil), "acme", "svc"); err == nil {
		t.Fatal("a 500 must be an error, not a silent denial")
	}
}

func TestRepoPushAccessWithToken_RequiresInputs(t *testing.T) {
	if _, err := RepoPushAccessWithToken(context.Background(), "", "acme", "svc"); err == nil {
		t.Fatal("a missing token must be rejected before any request")
	}
	if _, err := RepoPushAccessWithToken(context.Background(), "tok", "", "svc"); err == nil {
		t.Fatal("a missing owner must be rejected")
	}
}

// The bearer transport must not mutate the caller's request (a RoundTripper
// contract), and must actually attach the credential.
func TestBearerTransport_ClonesAndAuthorizes(t *testing.T) {
	var seen *http.Request
	inner := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})
	original, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/a/b", nil)
	if _, err := (bearerTransport{token: "secret-tok", base: inner}).RoundTrip(original); err != nil {
		t.Fatal(err)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer secret-tok" {
		t.Fatalf("the credential must be attached, got %q", got)
	}
	if original.Header.Get("Authorization") != "" {
		t.Fatal("a RoundTripper must not mutate the caller's request")
	}
}
