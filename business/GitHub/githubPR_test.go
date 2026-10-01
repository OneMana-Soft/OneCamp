package business

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc lets a test stand in for GitHub's HTTP transport with no network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockClient(status int, body string, capture *http.Request) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if capture != nil {
			*capture = *r
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestCreatePullRequestWithClient_Success(t *testing.T) {
	var got http.Request
	client := mockClient(http.StatusCreated,
		`{"number":42,"html_url":"https://github.com/o/r/pull/42","state":"open","draft":true}`, &got)

	pr, err := createPullRequestWithClient(context.Background(), client,
		"o", "r", "onecamp-agent/fix", "beta", "Fix padding", "body text", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.Number != 42 || pr.HTMLURL == "" || !pr.Draft {
		t.Fatalf("unexpected PR result: %+v", pr)
	}
	// Correct endpoint + method.
	if got.Method != http.MethodPost || got.URL.Path != "/repos/o/r/pulls" {
		t.Fatalf("wrong request target: %s %s", got.Method, got.URL.Path)
	}
}

func TestCreatePullRequestWithClient_AlreadyExists(t *testing.T) {
	client := mockClient(http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for o:branch."}]}`, nil)
	_, err := createPullRequestWithClient(context.Background(), client,
		"o", "r", "h", "base", "t", "b", false)
	if err != ErrPullRequestExists {
		t.Fatalf("expected ErrPullRequestExists, got %v", err)
	}
}

func TestCreatePullRequestWithClient_ErrorBounded(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	client := mockClient(http.StatusForbidden, huge, nil)
	_, err := createPullRequestWithClient(context.Background(), client, "o", "r", "h", "base", "t", "b", false)
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error message should be bounded, got %d chars", len(err.Error()))
	}
}

func TestCreatePullRequest_InputValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := CreatePullRequest(ctx, "", "r", "h", "b", "t", "body", false); err == nil {
		t.Fatal("expected error for missing owner")
	}
	if _, err := CreatePullRequest(ctx, "o", "r", "main", "main", "t", "body", false); err == nil {
		t.Fatal("head==base must be rejected (never open a PR onto itself)")
	}
	if _, err := CreatePullRequest(ctx, "o", "r", "", "b", "t", "body", false); err == nil {
		t.Fatal("expected error for missing head branch")
	}
}
