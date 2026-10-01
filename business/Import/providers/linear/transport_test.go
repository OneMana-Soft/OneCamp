package linear

// Tests for the transport layer (gql) — the most fragile part of the
// provider because it owns rate-limit handling, error envelope
// destructuring, and 4xx mapping. We drive everything through a
// httptest.Server so no Linear credentials are needed in CI.
//
// What this catches:
//   - Linear changing their {data, errors} envelope shape
//   - 429 + Retry-After translation regressions
//   - 401 → "reconnect" mapping regressions
//   - Authorization header drift (Linear is "Bearer <tok>", not raw)
//   - JSON encoding of variables (cursors, ids)
//   - Pagination loop termination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// withFakeLinear spins up a httptest server, points the package's
// graphqlEndpoint at it, and returns a teardown func. The
// graphqlEndpoint var swap is local to the test process; the package
// global resets via t.Cleanup so concurrent test packages can't observe
// the override.
func withFakeLinear(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	prev := graphqlEndpoint
	graphqlEndpoint = srv.URL
	t.Cleanup(func() {
		graphqlEndpoint = prev
		srv.Close()
	})
	return srv
}

func TestGQL_HappyPath_DecodesData(t *testing.T) {
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		// Validate the request shape: must be POST application/json
		// with the right Authorization header.
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer my-token" {
			t.Errorf("unexpected auth header: %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("unexpected content-type: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"hello": "world"}}`))
	})

	p := New()
	var out struct {
		Hello string `json:"hello"`
	}
	if err := p.gql(context.Background(), "my-token", `query { hello }`, nil, &out); err != nil {
		t.Fatalf("gql: %v", err)
	}
	if out.Hello != "world" {
		t.Fatalf("unexpected decoded data: %+v", out)
	}
}

func TestGQL_GraphQLErrorsBubble(t *testing.T) {
	// Linear returns 200 with an `errors` field for application-level
	// failures. The transport must surface those rather than returning
	// the (empty) data object silently.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": null, "errors": [{"message": "Issue not found"}]}`))
	})

	p := New()
	var out struct{}
	err := p.gql(context.Background(), "tok", `query{}`, nil, &out)
	if err == nil {
		t.Fatal("expected error from graphql errors envelope")
	}
	if !strings.Contains(err.Error(), "Issue not found") {
		t.Fatalf("expected message in error, got %v", err)
	}
}

func TestGQL_Returns429AsRateLimited(t *testing.T) {
	// 429 with Retry-After should surface as ErrRateLimited so the
	// orchestrator's chunk worker hits ResetChunkForRetry instead of
	// burning attempts.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	p := New()
	var out struct{}
	err := p.gql(context.Background(), "tok", `query{}`, nil, &out)
	if err == nil {
		t.Fatal("expected ErrRateLimited")
	}
	d, ok := importProvider.IsRateLimited(err)
	if !ok {
		t.Fatalf("expected ErrRateLimited, got %T %v", err, err)
	}
	if d != 7*time.Second {
		t.Fatalf("expected RetryAfter=7s, got %v", d)
	}
}

func TestGQL_401MapsToReconnect(t *testing.T) {
	// A 401 means the access token is no longer valid. We surface a
	// clear "reconnect" error so the FE can prompt the admin instead
	// of looping indefinitely.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	p := New()
	var out struct{}
	err := p.gql(context.Background(), "tok", `query{}`, nil, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "reconnect") {
		t.Fatalf("expected 'reconnect' hint, got %v", err)
	}
}

func TestGQL_ContextCancellationStopsRequest(t *testing.T) {
	// The transport must honour the caller's context. We cancel BEFORE
	// dispatching the request — that's the strict contract: a cancelled
	// ctx must never produce network traffic. Server-side ctx propagation
	// also works in practice but depends on TCP teardown timing that
	// makes the test brittle on slower CI runners.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not have been hit with a cancelled ctx")
	})

	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out struct{}
	err := p.gql(ctx, "tok", `query{}`, nil, &out)
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestGQL_VariablesRoundTrip(t *testing.T) {
	// Verifies the wire shape Linear sees: {query, variables}. A future
	// refactor could inadvertently nest variables incorrectly and the
	// API would silently return null data without errors — exactly the
	// kind of bug a mock server catches but a real one might not.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Variables["cursor"] != "cur-2" {
			t.Errorf("expected cursor=cur-2, got %v", body.Variables["cursor"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"ok": true}}`))
	})

	p := New()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := p.gql(context.Background(), "tok", `query($cursor: String) { x }`,
		map[string]any{"cursor": "cur-2"}, &out); err != nil {
		t.Fatalf("gql: %v", err)
	}
	if !out.OK {
		t.Fatal("expected decoded ok=true")
	}
}

func TestFetchUsers_PaginatesUntilHasNextPageFalse(t *testing.T) {
	// Two-page response: cursor advances, then HasNextPage=false ends
	// the loop. This is the exact shape Linear returns for users().
	var hits int32
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := atomic.AddInt32(&hits, 1)
		switch page {
		case 1:
			_, _ = w.Write([]byte(`{"data": {"users": {
				"nodes": [
					{"id": "u1", "name": "One"},
					{"id": "u2", "name": "Two"}
				],
				"pageInfo": {"hasNextPage": true, "endCursor": "cur-1"}
			}}}`))
		case 2:
			// Confirm the cursor is forwarded as a variable.
			body, _ := readBody(r)
			if !strings.Contains(body, `"cursor":"cur-1"`) {
				t.Errorf("expected cursor=cur-1 on page 2, got body %q", body)
			}
			_, _ = w.Write([]byte(`{"data": {"users": {
				"nodes": [
					{"id": "u3", "name": "Three"}
				],
				"pageInfo": {"hasNextPage": false, "endCursor": ""}
			}}}`))
		default:
			t.Errorf("unexpected page hit: %d", page)
		}
	})

	p := New()
	users, err := p.fetchUsers(context.Background(), "tok")
	if err != nil {
		t.Fatalf("fetchUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users across 2 pages, got %d", len(users))
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected 2 server hits, got %d", atomic.LoadInt32(&hits))
	}
}

// readBody is a small helper because the server handler closure can't
// take a *testing.T for io errors directly. We swallow errors here to
// keep the assertion code linear.
func readBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	defer r.Body.Close()
	buf := make([]byte, 4096)
	n, err := r.Body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return "", err
	}
	return string(buf[:n]), nil
}

func TestParseRetryAfter_HandlesHTTPDate(t *testing.T) {
	// Numeric form is covered by the existing helper test; HTTP-date
	// form is rare from Linear but legal under RFC 7231.
	future := time.Now().UTC().Add(45 * time.Second).Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got <= 0 || got > 60 {
		t.Fatalf("expected ~45s, got %d", got)
	}
}

func TestGQL_5xxErrorIncludesStatus(t *testing.T) {
	// Servers go down. A clear status-coded error makes log triage
	// possible without enabling verbose logging.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, "upstream down")
	})

	p := New()
	var out struct{}
	err := p.gql(context.Background(), "tok", `query{}`, nil, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected status code in error, got %v", err)
	}
}

func TestFetchTeamIssues_FlattensNestedShape(t *testing.T) {
	// The team(id).issues query returns issues with deeply nested
	// state/assignee/creator/parent/team/project objects plus
	// embedded comments + attachments. A schema drift on any of
	// those would silently produce zero values in our DTO.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "data": {
		    "team": {
		      "issues": {
		        "nodes": [
		          {
		            "id": "iss-1",
		            "identifier": "ENG-1",
		            "title": "Fix the navbar",
		            "description": "**bold**",
		            "url": "https://linear.app/acme/issue/ENG-1",
		            "priority": 2,
		            "priorityLabel": "High",
		            "createdAt": "2026-05-25T00:00:00Z",
		            "updatedAt": "2026-05-25T01:00:00Z",
		            "completedAt": null,
		            "canceledAt": null,
		            "startedAt": null,
		            "dueDate": "2026-06-30",
		            "state": {"id": "s-1", "name": "In Progress", "type": "started"},
		            "assignee": {"id": "u-1"},
		            "creator":  {"id": "u-2"},
		            "parent":   null,
		            "team":     {"id": "team-1"},
		            "project":  {"id": "proj-1"},
		            "labels":   {"nodes": [{"name": "bug"}, {"name": "frontend"}]},
		            "comments": {
		              "nodes": [
		                {"id": "c-1", "body": "lgtm", "createdAt": "2026-05-25T02:00:00Z", "user": {"id": "u-2"}}
		              ]
		            },
		            "attachments": {
		              "nodes": [
		                {"id": "a-1", "title": "screenshot.png", "url": "https://uploads.linear.app/x.png", "subtitle": ""}
		              ]
		            }
		          }
		        ],
		        "pageInfo": {"hasNextPage": false, "endCursor": ""}
		      }
		    }
		  }
		}`))
	})

	p := New()
	issues, err := p.fetchTeamIssues(context.Background(), "tok", "team-1")
	if err != nil {
		t.Fatalf("fetchTeamIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	iss := issues[0]
	if iss.ID != "iss-1" || iss.Identifier != "ENG-1" || iss.Title != "Fix the navbar" {
		t.Fatalf("flat fields wrong: %+v", iss)
	}
	if iss.StateID != "s-1" || iss.StateName != "In Progress" || iss.StateType != "started" {
		t.Fatalf("state flatten failed: id=%q name=%q type=%q", iss.StateID, iss.StateName, iss.StateType)
	}
	if iss.AssigneeID != "u-1" || iss.CreatorID != "u-2" {
		t.Fatalf("user flatten failed: assignee=%q creator=%q", iss.AssigneeID, iss.CreatorID)
	}
	if iss.TeamID != "team-1" || iss.ProjectID != "proj-1" {
		t.Fatalf("team/project flatten failed: team=%q project=%q", iss.TeamID, iss.ProjectID)
	}
	if iss.PriorityLabel != "High" {
		t.Fatalf("priority flatten failed: %q", iss.PriorityLabel)
	}
	if len(iss.Labels) != 2 || iss.Labels[0] != "bug" || iss.Labels[1] != "frontend" {
		t.Fatalf("labels flatten failed: %+v", iss.Labels)
	}
	if len(iss.Comments) != 1 || iss.Comments[0].ID != "c-1" || iss.Comments[0].UserID != "u-2" {
		t.Fatalf("comments flatten failed: %+v", iss.Comments)
	}
	if len(iss.Attachments) != 1 || iss.Attachments[0].URL != "https://uploads.linear.app/x.png" {
		t.Fatalf("attachments flatten failed: %+v", iss.Attachments)
	}
	if iss.DueDate == nil || iss.DueDate.Year() != 2026 || iss.DueDate.Month().String() != "June" {
		t.Fatalf("due date YYYY-MM-DD parse failed: %v", iss.DueDate)
	}
}

func TestFetchTeamIssues_NullParentDoesNotSetParentID(t *testing.T) {
	// Top-level issues come back with parent=null. The code must
	// leave ParentID empty (not crash, not panic, not stamp "null").
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "data": {
		    "team": {
		      "issues": {
		        "nodes": [
		          {
		            "id": "iss-1",
		            "identifier": "ENG-1",
		            "title": "top-level",
		            "createdAt": "2026-05-25T00:00:00Z",
		            "updatedAt": "2026-05-25T00:00:00Z",
		            "state": {"id": "s-1", "name": "Todo", "type": "unstarted"},
		            "parent": null,
		            "labels":   {"nodes": []},
		            "comments": {"nodes": []},
		            "attachments": {"nodes": []}
		          }
		        ],
		        "pageInfo": {"hasNextPage": false, "endCursor": ""}
		      }
		    }
		  }
		}`))
	})
	p := New()
	issues, err := p.fetchTeamIssues(context.Background(), "tok", "team-1")
	if err != nil {
		t.Fatalf("fetchTeamIssues: %v", err)
	}
	if issues[0].ParentID != "" {
		t.Fatalf("expected empty ParentID for top-level issue, got %q", issues[0].ParentID)
	}
}
