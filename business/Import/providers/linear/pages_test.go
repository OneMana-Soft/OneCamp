package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Linear answers a rate limit with HTTP 400 and the code RATELIMITED
// (linear.app/developers/rate-limiting), not 429. The import failed on it;
// it now waits until the window resets, which the reset headers give in
// epoch milliseconds.
func TestALinearRateLimitWaitsForTheReset(t *testing.T) {
	reset := time.Now().Add(42 * time.Second)
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))
	})
	err := New().gql(context.Background(), "tok", viewerQuery, nil, &struct{}{})
	var rl *importProvider.ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("400 RATELIMITED: %v, want a wait (ErrRateLimited)", err)
	}
	if rl.RetryAfter < 30*time.Second || rl.RetryAfter > 45*time.Second {
		t.Fatalf("waits %v, want about 42s (the requests window's reset)", rl.RetryAfter)
	}

	// The complexity window says when it resets the same way, and an
	// answer with no reset header still waits rather than failing.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Complexity limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))
	})
	if err := New().gql(context.Background(), "tok", viewerQuery, nil, &struct{}{}); !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("RATELIMITED with no reset header: %v, want a wait", err)
	}

	// Any other 400 is still an error, not a wait.
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Argument Validation Error","extensions":{"code":"INVALID_INPUT"}}]}`))
	})
	if err := New().gql(context.Background(), "tok", viewerQuery, nil, &struct{}{}); err == nil || errors.As(err, &rl) {
		t.Fatalf("a plain 400: %v, want an error that isn't a wait", err)
	}
}

// An issue brings a first page of its comments, files and blocking
// relations, and the rest come a page at a time from the issue itself.
// Before, comments past the 50th were dropped.
func TestAnIssuesCommentsArriveEveryPage(t *testing.T) {
	var asked []string
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "team(id: $teamId)"):
			asked = append(asked, "issues")
			_, _ = w.Write([]byte(`{"data":{"team":{"issues":{"nodes":[{
				"id":"iss-1","identifier":"ENG-1","title":"Talk","state":{"id":"s","name":"Todo","type":"unstarted"},
				"labels":{"nodes":[]},
				"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
				"attachments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
				"comments":{"nodes":[{"id":"c-1","body":"one","createdAt":"2026-10-01T09:00:00Z","user":{"id":"u"}}],
				            "pageInfo":{"hasNextPage":true,"endCursor":"cur-1"}}
			}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
		case strings.Contains(body.Query, "issue(id: $id)") && strings.Contains(body.Query, "comments("):
			asked = append(asked, "comments after "+toString(body.Variables["cursor"]))
			if body.Variables["id"] != "iss-1" || body.Variables["cursor"] != "cur-1" {
				t.Errorf("follow-up vars %v", body.Variables)
			}
			_, _ = w.Write([]byte(`{"data":{"issue":{"comments":{"nodes":[
				{"id":"c-2","body":"two","createdAt":"2026-10-01T10:00:00Z","user":{"id":"u"}},
				{"id":"c-3","body":"three","createdAt":"2026-10-01T11:00:00Z","user":null}
			],"pageInfo":{"hasNextPage":false,"endCursor":"cur-2"}}}}}`))
		default:
			t.Errorf("unexpected query: %s", body.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	issues, err := New().fetchTeamIssues(context.Background(), "tok", "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("%d issues", len(issues))
	}
	var ids []string
	for _, c := range issues[0].Comments {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "c-1,c-2,c-3" {
		t.Fatalf("comments %v (asked %v), want c-1,c-2,c-3", ids, asked)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
