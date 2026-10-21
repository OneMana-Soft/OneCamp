//go:build integration
// +build integration

package integration_test

// The public API's one shape, against real Redis (business/PublicAPI;
// design/platform/API_CONTRACT.md §3.2): a write sent again under the same
// Idempotency-Key runs once and is answered from its first answer, the key
// with another body is refused, and a route holds each token to its own
// limit. The routes here are the test's own ("/test-…", which
// TestEveryPublicAPIRouteAnswers leaves out); every registered route gets the
// same middleware.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAPublicWrite|TestARouteHolds' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

var (
	shapeProbesOnce sync.Once
	echoRuns        atomic.Int32
)

// registerShapeProbes adds the test's own routes, once per test binary.
func registerShapeProbes() {
	shapeProbesOnce.Do(func() {
		v1api.Register(v1api.Resource{
			Name: "test-shape", Owner: "platform",
			Routes: []v1api.Route{
				{Method: http.MethodPost, Path: "/test-echo", Scope: "tasks:write", Write: true,
					Serve: func(_ context.Context, req v1api.Request) (any, error) {
						n := echoRuns.Add(1)
						return map[string]any{"run": n, "body": string(req.Body)}, nil
					}},
				{Method: http.MethodGet, Path: "/test-limited", Scope: "tasks:read", Limit: v1api.Limit{PerMinute: 3},
					Serve: func(context.Context, v1api.Request) (any, error) { return map[string]bool{"ok": true}, nil }},
			},
		})
	})
}

// probeRouter mounts the registry under a stand-in for the token check: the
// token's id and scopes come from the test's headers.
func probeRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := context.WithValue(req.Context(), helpers.ApiTokenIDContextKey, req.Header.Get("X-Test-Token"))
			ctx = context.WithValue(ctx, helpers.ApiScopesContextKey, strings.Split(req.Header.Get("X-Test-Scopes"), ","))
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	v1api.Mount(r, v1api.Middleware{RequireScope: customMiddleware.RequireScope})
	return r
}

func probe(h http.Handler, method, path, token, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Test-Token", token)
	req.Header.Set("X-Test-Scopes", "tasks:read,tasks:write")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestAPublicWriteUnderOneIdempotencyKeyRunsOnce(t *testing.T) {
	integration.SetupRedis(t)
	registerShapeProbes()
	h := probeRouter()
	before := echoRuns.Load()

	first, a := probe(h, "POST", "/test-echo", "tok-a", "k-1", `{"n":1}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("the first send answered %d: %v", first.Code, a)
	}
	again, b := probe(h, "POST", "/test-echo", "tok-a", "k-1", `{"n":1}`)
	if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" || b["run"] != a["run"] {
		t.Errorf("the same send again answered %d %v (replayed %q), the first %v", again.Code, b, again.Header().Get("Idempotent-Replayed"), a)
	}
	if got := echoRuns.Load() - before; got != 1 {
		t.Errorf("one key ran %d times", got)
	}

	other, c := probe(h, "POST", "/test-echo", "tok-a", "k-1", `{"n":2}`)
	if other.Code != http.StatusUnprocessableEntity || c["error"].(map[string]any)["code"] != "idempotency_key_reused" {
		t.Errorf("the key with another body answered %d %v", other.Code, c)
	}

	// Another key, or the same key from another token, is another write.
	probe(h, "POST", "/test-echo", "tok-a", "k-2", `{"n":1}`)
	probe(h, "POST", "/test-echo", "tok-b", "k-1", `{"n":1}`)
	if got := echoRuns.Load() - before; got != 3 {
		t.Errorf("three keys between two tokens ran %d times", got)
	}

	// No key: every send runs.
	probe(h, "POST", "/test-echo", "tok-a", "", `{"n":1}`)
	probe(h, "POST", "/test-echo", "tok-a", "", `{"n":1}`)
	if got := echoRuns.Load() - before; got != 5 {
		t.Errorf("two sends without a key ran %d times in all", got)
	}
}

func TestARouteHoldsATokenToItsOwnLimit(t *testing.T) {
	integration.SetupRedis(t)
	registerShapeProbes()
	h := probeRouter()
	for i := 1; i <= 3; i++ {
		if w, out := probe(h, "GET", "/test-limited", "tok-limit", "", ""); w.Code != http.StatusOK {
			t.Fatalf("call %d of 3 answered %d: %v", i, w.Code, out)
		}
	}
	over, out := probe(h, "GET", "/test-limited", "tok-limit", "", "")
	if over.Code != http.StatusTooManyRequests || over.Header().Get("Retry-After") == "" || out["error"].(map[string]any)["code"] != "rate_limited" {
		t.Errorf("the fourth call answered %d %v (Retry-After %q)", over.Code, out, over.Header().Get("Retry-After"))
	}
	if w, _ := probe(h, "GET", "/test-limited", "tok-other", "", ""); w.Code != http.StatusOK {
		t.Errorf("another token was held to the first one's limit: %d", w.Code)
	}
}
