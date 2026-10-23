package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	appBusiness "github.com/akashc777/OneCamp/business/App"
)

// The public API's one shape (design/platform/API_CONTRACT.md §3.2): what
// Register refuses, and what Mount gives every route.

func resetForTest(t *testing.T) {
	t.Helper()
	mu.Lock()
	saved, savedScopes := resources, scopes
	resources, scopes = nil, map[string]ScopeDecl{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		resources, scopes = saved, savedScopes
		mu.Unlock()
	})
}

func panics(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = r.(string)
		}
	}()
	f()
	return ""
}

func ok(http.ResponseWriter, *http.Request) {}

func TestRegisterRefusesARouteThatIsOpenOrAmbiguous(t *testing.T) {
	resetForTest(t)
	cases := map[string]Route{
		"no scope at all":       {Method: "GET", Path: "/x", Handler: ok},
		"two ways to be scoped": {Method: "GET", Path: "/x", Scope: "a:read", AnyToken: true, Handler: ok},
		"no handler":            {Method: "GET", Path: "/x", Scope: "a:read"},
		"two handlers":          {Method: "GET", Path: "/x", Scope: "a:read", Handler: ok, Serve: func(context.Context, Request) (any, error) { return nil, nil }},
		// How an app's token is held there: a route never opens to apps by
		// being forgotten.
		"no word on apps":    {Method: "GET", Path: "/x", Scope: "a:read", Handler: ok},
		"two words on apps":  {Method: "GET", Path: "/x", Scope: "a:read", AppsInside: true, AppsRefused: "no", Handler: ok},
		"a place and a word": {Method: "GET", Path: "/x", Scope: "a:read", Reach: func(*http.Request) (string, string) { return "", "" }, AppsInside: true, Handler: ok},
	}
	for name, rt := range cases {
		if panics(func() { Register(Resource{Name: "x", Routes: []Route{rt}}) }) == "" {
			t.Errorf("%s: registered", name)
		}
	}
	Register(Resource{Name: "a", Routes: []Route{{Method: "GET", Path: "/a", Scope: "a:read", AppsRefused: "no", Handler: ok}}})
	if msg := panics(func() {
		Register(Resource{Name: "b", Routes: []Route{{Method: "GET", Path: "/a", Scope: "b:read", AppsRefused: "no", Handler: ok}}})
	}); !strings.Contains(msg, "twice") {
		t.Errorf("a route registered twice was taken: %q", msg)
	}
}

// requireHeaderScope stands in for the token middleware: the scopes a test
// request carries are in X-Test-Scopes.
func requireHeaderScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(","+r.Header.Get("X-Test-Scopes")+",", ","+scope+",") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type versioned struct {
	Name string `json:"name"`
	V    int64  `json:"version"`
}

func (v versioned) APIVersion() int64 { return v.V }

func mounted(t *testing.T, res ...Resource) *chi.Mux {
	t.Helper()
	resetForTest(t)
	for _, r := range res {
		Register(r)
	}
	r := chi.NewRouter()
	Mount(r, Middleware{RequireScope: requireHeaderScope})
	return r
}

func send(h http.Handler, method, path, scopes, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if scopes != "" {
		req.Header.Set("X-Test-Scopes", scopes)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) Error {
	t.Helper()
	var body struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not the one error body: %s", w.Body)
	}
	return body.Error
}

func TestMountGivesEveryRouteTheShape(t *testing.T) {
	gated := 0
	r := mounted(t, Resource{
		Name: "things", Owner: "test",
		Routes: []Route{
			{Method: "GET", Path: "/things/{id}", Scope: "things:read", Versioned: true, AppsInside: true,
				Serve: func(_ context.Context, req Request) (any, error) {
					if chi.URLParam(req.HTTP, "id") == "gone" {
						return nil, Errorf(http.StatusNotFound, "not_found", "check the id", "there's no such thing")
					}
					if chi.URLParam(req.HTTP, "id") == "broken" {
						return nil, errors.New("pq: relation \"things\" does not exist")
					}
					return versioned{Name: "a thing", V: 7}, nil
				}},
			{Method: "POST", Path: "/things", Scope: "things:write", Write: true, AppsInside: true,
				Serve: func(_ context.Context, req Request) (any, error) {
					return map[string]string{"got": string(req.Body)}, nil
				}},
			{Method: "GET", Path: "/old", Scope: "things:read", Legacy: true, AppsInside: true,
				Use: []func(http.Handler) http.Handler{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gated++; next.ServeHTTP(w, r) })
				}},
				Handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"msg":"as it was"}`) }},
			{Method: "GET", Path: "/me", AnyToken: true, AppsInside: true, Handler: ok},
		},
	})

	w := send(r, "GET", "/things/1", "things:read", "")
	if w.Code != 200 || w.Header().Get("X-OneCamp-API-Version") != Version || w.Header().Get("X-Request-Id") == "" {
		t.Fatalf("a read answered %d with headers %v", w.Code, w.Header())
	}
	if got := w.Header().Get("ETag"); got != `W/"7"` {
		t.Errorf("a versioned answer's ETag is %q", got)
	}
	if w := send(r, "GET", "/things/1", "", ""); w.Code != http.StatusForbidden {
		t.Errorf("a token without the scope got %d", w.Code)
	}
	if w := send(r, "GET", "/me", "", ""); w.Code != http.StatusOK {
		t.Errorf("/me, open to any token, got %d", w.Code)
	}
	if w := send(r, "GET", "/things/1", "things:read", "", "X-Request-Id", "req-42"); w.Header().Get("X-Request-Id") != "req-42" {
		t.Error("the client's request id wasn't kept")
	}

	gone := send(r, "GET", "/things/gone", "things:read", "")
	if e := errorOf(t, gone); gone.Code != 404 || e.Code != "not_found" || e.Hint != "check the id" {
		t.Errorf("a not-found answered %d %+v", gone.Code, e)
	}
	broken := send(r, "GET", "/things/broken", "things:read", "")
	if e := errorOf(t, broken); broken.Code != 500 || e.Code != "internal" || strings.Contains(broken.Body.String(), "pq:") {
		t.Errorf("an internal failure answered %d and said %s", broken.Code, broken.Body)
	}

	made := send(r, "POST", "/things", "things:write", `{"name":"b"}`)
	if made.Code != http.StatusCreated || !strings.Contains(made.Body.String(), `"got"`) {
		t.Errorf("a write answered %d %s", made.Code, made.Body)
	}

	old := send(r, "GET", "/old", "things:read", "")
	if old.Body.String() != `{"msg":"as it was"}` || gated != 1 || old.Header().Get("X-OneCamp-API-Version") != Version {
		t.Errorf("a legacy route answered %q (its gate ran %d times)", old.Body, gated)
	}
}

func TestWithoutRedisAWriteStillRuns(t *testing.T) {
	// No Redis here: the limits and Idempotency-Key fail open, as the token's
	// own limit does, so an outage never takes the API down.
	var runs atomic.Int32
	r := mounted(t, Resource{Name: "w", Routes: []Route{{Method: "POST", Path: "/w", Scope: "w:write", Write: true, AppsInside: true,
		Serve: func(context.Context, Request) (any, error) {
			runs.Add(1)
			return map[string]int{"n": int(runs.Load())}, nil
		}}}})
	for i := 0; i < 2; i++ {
		if w := send(r, "POST", "/w", "w:write", `{}`, "Idempotency-Key", "k1"); w.Code != http.StatusCreated {
			t.Fatalf("answered %d", w.Code)
		}
	}
	if runs.Load() != 2 {
		t.Errorf("ran %d times", runs.Load())
	}
}

func TestAReachedRouteLetsEveryTokenButAnAppThrough(t *testing.T) {
	// No app is on these requests' context, so the gate asks nothing: a
	// person's or an agent's token is unaffected (an app's refusal is
	// public_api_app_test.go's, against real stores).
	asked := 0
	r := mounted(t, Resource{Name: "reach", Routes: []Route{{Method: "GET", Path: "/reach/{id}", Scope: "r:read",
		Reach:   func(r *http.Request) (string, string) { asked++; return "table", chi.URLParam(r, "id") },
		Handler: ok}}})
	if w := send(r, "GET", "/reach/7", "r:read", ""); w.Code != http.StatusOK || asked != 0 {
		t.Errorf("a token that isn't an app's answered %d (the gate asked %d times)", w.Code, asked)
	}
}

// An app is held to the places it was added to on every route (business/App,
// A10): a route names its place, which may be in the body; or its handler
// holds the app itself; or the app is refused in the route's own words. A
// failed read of the app opens nothing, and other tokens are unaffected.
func TestEveryRouteHoldsAnAppToItsPlaces(t *testing.T) {
	app := &appBusiness.Identity{Name: "Deploy bot", Audience: appBusiness.Audience{Places: []appBusiness.Place{{Kind: "channel", ID: "general"}}}}
	acting, readErr := app, error(nil)
	prev := actsAsApp
	t.Cleanup(func() { actsAsApp = prev })
	actsAsApp = func(context.Context) (*appBusiness.Identity, error) { return acting, readErr }
	ran := 0
	echo := func(w http.ResponseWriter, r *http.Request) {
		ran++
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	}
	inBody := func(r *http.Request) (string, string) {
		body, _ := PeekBody(r)
		var b struct {
			Channel string `json:"channel"`
		}
		_ = json.Unmarshal(body, &b)
		return "channel", b.Channel
	}
	r := mounted(t, Resource{Name: "places", Routes: []Route{
		{Method: "POST", Path: "/post", Scope: "p:write", Write: true, Reach: inBody, Handler: echo},
		{Method: "GET", Path: "/mine", Scope: "p:read", AppsInside: true, Handler: echo},
		{Method: "GET", Path: "/unread", Scope: "p:read", AppsRefused: "Unread counts are a person's; an app has none.", Handler: echo},
	}})

	for _, c := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"its own channel, named in the body", "POST", "/post", `{"channel":"general"}`, http.StatusOK, ""},
		{"another channel", "POST", "/post", `{"channel":"exec"}`, http.StatusForbidden, "not_in_audience"},
		{"no channel", "POST", "/post", `{}`, http.StatusForbidden, "not_in_audience"},
		{"a route that holds it itself", "GET", "/mine", "", http.StatusOK, ""},
		{"a route closed to apps", "GET", "/unread", "", http.StatusForbidden, "not_for_apps"},
	} {
		ran = 0
		w := send(r, c.method, c.path, "p:read,p:write", c.body)
		if w.Code != c.status || (c.code == "") != (ran == 1) {
			t.Errorf("%s: %d %s, the handler ran %d times", c.name, w.Code, w.Body, ran)
			continue
		}
		if c.code != "" {
			if e := errorOf(t, w); e.Code != c.code {
				t.Errorf("%s: %+v, want %s", c.name, e, c.code)
			}
		} else if w.Body.String() != c.body {
			t.Errorf("%s: the handler read %q, want the whole body %q", c.name, w.Body, c.body)
		}
	}
	if e := errorOf(t, send(r, "GET", "/unread", "p:read", "")); e.Message != "Unread counts are a person's; an app has none." {
		t.Errorf("a route closed to apps refused with %q, not its own words", e.Message)
	}

	// The app's record couldn't be read: refused, never let through as "not an app".
	readErr, acting = errors.New("pq: connection refused"), nil
	for _, path := range []string{"/mine", "/unread"} {
		ran = 0
		if w := send(r, "GET", path, "p:read", ""); w.Code != http.StatusServiceUnavailable || ran != 0 || strings.Contains(w.Body.String(), "pq:") {
			t.Errorf("%s while the app can't be read: %d %s, the handler ran %d times", path, w.Code, w.Body, ran)
		}
	}

	// A person's or an agent's token: no app, nothing held.
	readErr = nil
	ran = 0
	if w := send(r, "GET", "/unread", "p:read", ""); w.Code != http.StatusOK || ran != 1 {
		t.Errorf("a token that isn't an app's, on a route closed to apps: %d, the handler ran %d times", w.Code, ran)
	}
}

func TestOpenAPIDescribesEveryRegisteredRoute(t *testing.T) {
	resetForTest(t)
	Register(Resource{Name: "things", Owner: "test", Scopes: []ScopeDecl{{Name: "things:write", Description: "Make things"}},
		Routes: []Route{
			{Method: "POST", Path: "/things/{id}/parts", Summary: "Add a part", Scope: "things:write", Write: true, Versioned: true, AppsInside: true, Handler: ok},
			{Method: "GET", Path: "/legacy", Scope: "things:read", Legacy: true, AppsInside: true, Handler: ok},
		}})
	doc := OpenAPI()
	paths := doc["paths"].(map[string]any)
	post := paths["/things/{id}/parts"].(map[string]any)["post"].(map[string]any)
	if post["x-scope"] != "things:write" || post["summary"] != "Add a part" || post["x-owner"] != "test" {
		t.Errorf("the write's operation: %v", post)
	}
	names := []string{}
	for _, p := range post["parameters"].([]map[string]any) {
		names = append(names, p["name"].(string))
	}
	if strings.Join(names, ",") != "id,Idempotency-Key,If-Match" {
		t.Errorf("the write's parameters: %v", names)
	}
	if paths["/legacy"].(map[string]any)["get"].(map[string]any)["x-legacy"] != true {
		t.Error("a legacy route isn't marked")
	}
	if doc["x-scopes"].(map[string]string)["things:write"] != "Make things" {
		t.Errorf("declared scopes: %v", doc["x-scopes"])
	}
}

// The description names the addresses an integration uses outside the token
// routes, and says an incoming webhook runs no commands, so nobody builds on
// the slash commands it used to take.
func TestOpenAPINamesTheCapabilityURLsAndThatWebhooksRunNoCommands(t *testing.T) {
	urls, _ := OpenAPI()["x-capability-urls"].(map[string]string)
	hook, response := urls["POST /webhook/incoming/{token}"], urls["POST /command-responses/{token}"]
	if !strings.Contains(hook, "runs no commands") || response == "" {
		t.Fatalf("the capability URLs: %v", urls)
	}
}
