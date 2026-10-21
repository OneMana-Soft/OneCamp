package router

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
)

// The public API has one way in (platform P-API, 17 Oct 2026): a route is
// registered with business/PublicAPI by the package that owns it, and
// router.go mounts the registry. Mounted by hand, a route skipped its limit,
// Idempotency-Key and the reference, and nothing would say so.

// Every /v1 route there was before the registry, with its scope: the move took
// them all, as they were.
var legacyRoutes = map[string]string{
	"GET /me":                     "any token",
	"GET /unread":                 "attention:read",
	"GET /attention":              "attention:read",
	"GET /tables":                 "tables:read",
	"GET /tables/{id}":            "tables:read",
	"POST /tables/{id}/aggregate": "tables:read",
	"POST /tables/{id}/rows":      "tables:write",
	"GET /tasks":                  "tasks:read",
	"POST /tasks":                 "tasks:write",
	"POST /tasks/{id}/status":     "tasks:write",
	"GET /projects":               "projects:read",
	"POST /messages/channel":      "messages:write",
	"POST /messages/dm":           "messages:write",
	"GET /search":                 "search:read",
	"POST /search":                "search:read",
	"GET /docs":                   "docs:read",
	"GET /docs/{id}":              "docs:read",
	"POST /docs":                  "docs:write",
	"POST /docs/{id}/append":      "docs:write",
	"POST /mcp":                   "per call",
}

func TestEveryPublicRouteIsRegisteredWithItsScope(t *testing.T) {
	got := map[string]string{}
	for _, res := range v1api.Resources() {
		for _, rt := range res.Routes {
			scope := rt.Scope
			switch {
			case rt.AnyToken:
				scope = "any token"
			case rt.ScopeInside:
				scope = "per call"
			}
			got[rt.Method+" "+rt.Path] = scope
			if res.Owner == "" {
				t.Errorf("%s %s (%s) has no owner", rt.Method, rt.Path, res.Name)
			}
		}
	}
	for route, scope := range legacyRoutes {
		// /attention is the AI edition's: the edition without AI doesn't have it.
		if route == "GET /attention" && got[route] == "" {
			continue
		}
		if got[route] != scope {
			t.Errorf("%s: registered with %q, was %q", route, got[route], scope)
		}
	}
}

// handMounted is a /v1 route added on a router directly: v1Router.Get(...),
// v1.With(...).Post(...), rt.V1.Method(...).
var handMounted = regexp.MustCompile(`\b(v1Router|v1|rt\.V1)\s*\.\s*(Get|Post|Put|Patch|Delete|Method|Handle|HandleFunc|With|Route|Group)\s*\(`)

func TestNothingMountsAPublicRouteByHand(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if handMounted.MatchString(line) {
				found = append(found, f+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("register these with business/PublicAPI instead (v1api.Register, in the package that owns them):\n%s", strings.Join(found, "\n"))
	}
	// The one way in is there.
	router, _ := os.ReadFile("router.go")
	if !strings.Contains(string(router), "v1api.Mount(v1Router") {
		t.Error("router.go no longer mounts the registry")
	}
}
