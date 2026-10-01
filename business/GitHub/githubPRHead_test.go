package business

import (
	"context"
	"net/http"
	"testing"
)

func TestPullRequestHeadWithClient(t *testing.T) {
	const open = `{"state":"open","merged":false,"html_url":"https://github.com/acme/svc/pull/7","head":{"ref":"onecamp-agent/fix-1","repo":{"full_name":"acme/svc"}},"base":{"ref":"main"}}`
	const merged = `{"state":"closed","merged":true,"head":{"ref":"onecamp-agent/fix-1","repo":{"full_name":"acme/svc"}},"base":{"ref":"main"}}`
	const closed = `{"state":"closed","merged":false,"head":{"ref":"onecamp-agent/fix-1","repo":{"full_name":"acme/svc"}},"base":{"ref":"main"}}`
	const goneFork = `{"state":"open","merged":false,"head":{"ref":"patch-1","repo":null},"base":{"ref":"main"}}`

	cases := map[string]struct {
		status           int
		body             string
		found, open      bool
		headRef, headRep string
	}{
		"open":         {200, open, true, true, "onecamp-agent/fix-1", "acme/svc"},
		"merged":       {200, merged, true, false, "onecamp-agent/fix-1", "acme/svc"},
		"closed":       {200, closed, true, false, "onecamp-agent/fix-1", "acme/svc"},
		"deleted fork": {200, goneFork, true, true, "patch-1", ""},
		"not found":    {404, `{}`, false, false, "", ""},
		"forbidden":    {403, `{}`, false, false, "", ""},
	}
	for name, tc := range cases {
		var req http.Request
		got, err := pullRequestHeadWithClient(context.Background(), mockClient(tc.status, tc.body, &req), "acme", "svc", 7)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if req.URL.Path != "/repos/acme/svc/pulls/7" {
			t.Errorf("%s: asked for %s", name, req.URL.Path)
		}
		if got.Found != tc.found || got.Open != tc.open || got.HeadRef != tc.headRef || got.HeadRepo != tc.headRep {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestPullRequestHeadWithClient_UnexpectedStatusIsAnError(t *testing.T) {
	if _, err := pullRequestHeadWithClient(context.Background(), mockClient(500, `{}`, nil), "acme", "svc", 7); err == nil {
		t.Fatal("a 500 must be an error, not a missing pull request")
	}
	if _, err := pullRequestHeadWithClient(context.Background(), mockClient(200, `not json`, nil), "acme", "svc", 7); err == nil {
		t.Fatal("an unreadable body must be an error")
	}
}

func TestPullRequestHeadWithToken_RequiresInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := PullRequestHeadWithToken(ctx, "t", "", "svc", 1); err == nil {
		t.Fatal("owner required")
	}
	if _, err := PullRequestHeadWithToken(ctx, "t", "acme", "svc", 0); err == nil {
		t.Fatal("number required")
	}
	if _, err := PullRequestHeadWithToken(ctx, " ", "acme", "svc", 1); err == nil {
		t.Fatal("token required")
	}
}
