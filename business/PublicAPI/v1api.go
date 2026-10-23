// Package business (PublicAPI) is the public API's one shape (platform P-API;
// design/platform/API_CONTRACT.md §3.2, signed by chat and projects on 17 Oct
// 2026). Every /v1 route is registered here by the package that owns it, and
// Mount gives each the same treatment:
//
//   - its scope (or, for MCP, the handler's own per-tool scopes);
//   - a limit per token and route, beside the token's own (ApiTokenRateLimit),
//     with Retry-After;
//   - Idempotency-Key on writes: the same key with the same body is answered
//     from the first answer, with another body refused, for 24 hours;
//   - for routes in the new shape: one error body ({"error": {code, message,
//     hint}}), one page (Page: items, next_cursor), and an ETag where the
//     value has a version (the page and If-Match readers arrive with the
//     first route in the new shape that pages or versions, §3.2);
//   - an app held to the places it was added to (business/App): a route
//     names what it reaches (Reach), or holds the app itself (AppsInside), or
//     refuses it (AppsRefused);
//   - a request id and the API's version on every answer;
//
// and OpenAPI describes them all (/v1/openapi.json). Routes that existed
// before the registry moved into it as Legacy: their paths, answers and error
// bodies are as they were (R4); their owners move them to the new shape.
//
// An app's and an agent's tokens are API tokens: whatever a route reaches, a
// token acts at most as a member (helpers.ActsThroughToken, R1).
package business

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// The API's version, on every answer (R6: /v1 only gains; a break is /v2).
const Version = "1"

// The default limits per token and route, a minute (§6). A route can ask for
// fewer; the token's own limit (ApiTokenRateLimit) still applies on top.
const (
	DefaultReadsPerMinute  = 600
	DefaultWritesPerMinute = 120
)

// How long a write's answer is kept under its Idempotency-Key.
const idempotencyFor = 24 * time.Hour

// maxIdempotencyKey bounds the header a client sends.
const maxIdempotencyKey = 200

// Limit is a route's own allowance per token.
type Limit struct {
	PerMinute int
}

// ScopeDecl is a scope a resource brings, as people see it when they make a
// token.
type ScopeDecl struct {
	Name        string
	Description string
}

// Route is one method and path of a resource.
type Route struct {
	Method, Path string
	// Summary is one line for the reference (OpenAPI).
	Summary string
	// Scope the token must carry. Required, unless ScopeInside.
	Scope string
	// ScopeInside: the handler checks scopes itself, per call (MCP's tools).
	ScopeInside bool
	// AnyToken: any valid token may call it, because it reads only the token's
	// own owner (/me). Never with a Scope.
	AnyToken bool
	// Write: changes something. Writes take Idempotency-Key and the write limit.
	Write bool
	// Versioned: answers carry an ETag (the value's APIVersion).
	Versioned bool
	Limit     Limit
	// Legacy: moved into the registry as it was, answers and errors unchanged.
	Legacy bool
	// Use runs before the handler, after the scope and the limit (a policy gate).
	Use []func(http.Handler) http.Handler
	// Reach names the resource a request touches (its kind and id, from the
	// path, or from the body through PeekBody): an app's call is refused unless
	// the app was added there (business/App MayReach). A person's or an agent's
	// token is unaffected.
	Reach func(r *http.Request) (kind, id string)
	// A route with no Reach says how an app's token is held instead, in one of
	// two ways (Register insists, so no route opens to apps by being forgotten):
	// AppsInside, when its handler holds the app to its places itself (a list
	// cut to them, business/App Narrow) or reads only the app's own (/me); or
	// AppsRefused, the words an app is refused with.
	AppsInside  bool
	AppsRefused string
	// Handler answers the request itself (a legacy route, or one that streams).
	Handler http.HandlerFunc
	// Serve answers in the new shape: its value is written as JSON (200, or
	// 201 for a write), its error as the one error body.
	Serve func(ctx context.Context, req Request) (any, error)
}

// Resource is what one package brings to the API.
type Resource struct {
	Name  string // "tasks"
	Owner string // the programme: "projects"
	// Scopes it declares; tokens may be made with them.
	Scopes []ScopeDecl
	Routes []Route
	// Events are the webhook kinds it sends, for the reference.
	Events []string
}

// Request is what a route in the new shape is handed.
type Request struct {
	HTTP *http.Request
	// Body is the request's body, read once (writes; at most the API's body limit).
	Body []byte
}

// Error is the API's one error body: {"error": {"code", "message", "hint"}}.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf is an Error with a status, a code and words for people.
func Errorf(status int, code, hint, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// Page is the API's one page of a list.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Versioned is a value with a version, for its ETag.
type Versioned interface {
	APIVersion() int64
}

var (
	mu        sync.Mutex
	resources []Resource
	scopes    = map[string]ScopeDecl{}
)

// Register adds a resource, at its package's init. A route with neither a
// scope nor ScopeInside, one that doesn't say how an app's token is held, or
// one registered twice, panics: the API is a contract, and a route nobody gave
// a scope would be open to every token, one nobody held apps on open to apps.
func Register(r Resource) {
	mu.Lock()
	defer mu.Unlock()
	for _, rt := range r.Routes {
		if !oneOf(rt.Scope != "", rt.ScopeInside, rt.AnyToken) {
			panic(fmt.Sprintf("v1api: %s %s (%s) needs exactly one of Scope, ScopeInside or AnyToken", rt.Method, rt.Path, r.Name))
		}
		if !oneOf(rt.Reach != nil, rt.AppsInside, rt.AppsRefused != "") {
			panic(fmt.Sprintf("v1api: %s %s (%s) needs exactly one of Reach, AppsInside or AppsRefused", rt.Method, rt.Path, r.Name))
		}
		if (rt.Handler == nil) == (rt.Serve == nil) {
			panic(fmt.Sprintf("v1api: %s %s (%s) needs one of Handler or Serve", rt.Method, rt.Path, r.Name))
		}
		for _, other := range resources {
			for _, o := range other.Routes {
				if o.Method == rt.Method && o.Path == rt.Path {
					panic(fmt.Sprintf("v1api: %s %s is registered twice (%s, %s)", rt.Method, rt.Path, other.Name, r.Name))
				}
			}
		}
	}
	for _, s := range r.Scopes {
		scopes[s.Name] = s
		// A token may be made with every scope a resource declares.
		apiTokenBusiness.RegisterScope(s.Name)
	}
	resources = append(resources, r)
}

// oneOf is whether exactly one of choices holds.
func oneOf(choices ...bool) bool {
	n := 0
	for _, c := range choices {
		if c {
			n++
		}
	}
	return n == 1
}

// Resources are every registered resource, by name.
func Resources() []Resource {
	mu.Lock()
	defer mu.Unlock()
	out := append([]Resource(nil), resources...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Scopes are the scopes the registered resources declare.
func Scopes() map[string]ScopeDecl {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]ScopeDecl, len(scopes))
	for k, v := range scopes {
		out[k] = v
	}
	return out
}

// Router is where Mount puts the routes (a chi.Router is one).
type Router interface {
	Method(method, pattern string, h http.Handler)
}

// Middleware is the HTTP plumbing Mount needs from the router's package, so
// this one needs none of it.
type Middleware struct {
	RequireScope func(scope string) func(http.Handler) http.Handler
}

// Mount puts every registered route on r, under the router's token check, and
// the reference at GET /openapi.json.
func Mount(r Router, mw Middleware) {
	for _, res := range Resources() {
		for _, rt := range res.Routes {
			r.Method(rt.Method, rt.Path, chain(res, rt, mw))
		}
	}
	r.Method(http.MethodGet, "/openapi.json", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		stamp(w, req)
		helpers.WriteJSON(w, http.StatusOK, OpenAPI())
	}))
}

// chain is a route's handler with everything Mount gives it, outermost first:
// the version and request id, the scope, the route's limit, its own gates,
// then Idempotency-Key for a write.
func chain(res Resource, rt Route, mw Middleware) http.Handler {
	var h http.Handler = rt.Handler
	if rt.Serve != nil {
		h = serve(rt)
	}
	if rt.Write {
		h = idempotent(rt, h)
	}
	for i := len(rt.Use) - 1; i >= 0; i-- {
		h = rt.Use[i](h)
	}
	h = reached(rt, h)
	h = limited(rt, h)
	if rt.Scope != "" && mw.RequireScope != nil {
		h = mw.RequireScope(rt.Scope)(h)
	}
	inner := h
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamp(w, r)
		inner.ServeHTTP(w, r)
	})
}

// actsAsApp is the app a request's token belongs to: a seam for tests, which
// have no app records.
var actsAsApp = appBusiness.ActsAsApp

// reached holds an app's call to the places the app was added to (R1: an app
// is only ever narrowed), on every route: one that names its place asks
// MayReach, one whose handler holds the app itself lets it through, and any
// other refuses it in its own words. When what the app may reach can't be
// read, the call is refused: a failed read never opens a route. Nothing
// changes for any other token.
func reached(rt Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, err := actsAsApp(r.Context())
		if err != nil {
			helpers.LogErrorWithContext(r.Context(), "business/PublicAPI reached %s %s: %+v", rt.Method, rt.Path, err)
			writeError(w, Errorf(http.StatusServiceUnavailable, "unavailable", "try again shortly",
				"what this app may reach couldn't be checked"))
			return
		}
		if app != nil {
			switch {
			case rt.Reach != nil:
				kind, id := rt.Reach(r)
				if !appBusiness.MayReach(r.Context(), app, kind, id) {
					writeError(w, Errorf(http.StatusForbidden, "not_in_audience", "an admin can add it to the app in Admin › Apps",
						"%s wasn't added to this %s", app.Name, kind))
					return
				}
			case !rt.AppsInside:
				writeError(w, Errorf(http.StatusForbidden, "not_for_apps", "an app reaches only the places an admin added it to",
					"%s", rt.AppsRefused))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// PeekBody is a request's body, read and put back for the handler, as a Reach
// that takes its place from a write's body reads it.
func PeekBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

// stamp puts the API's version and a request id on the answer.
func stamp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-OneCamp-API-Version", Version)
	id := r.Header.Get("X-Request-Id")
	if id == "" || len(id) > 100 {
		id = uuid.NewString()
	}
	w.Header().Set("X-Request-Id", id)
}

// routeKey names a route for its limit and its idempotency keys.
func routeKey(rt Route) string { return rt.Method + " " + rt.Path }

// perMinute is a route's allowance.
func perMinute(rt Route) int {
	if rt.Limit.PerMinute > 0 {
		return rt.Limit.PerMinute
	}
	if rt.Write {
		return DefaultWritesPerMinute
	}
	return DefaultReadsPerMinute
}

// limited holds a token to the route's allowance. Without Redis it lets the
// call through, as the token's own limit does: an outage never takes the API
// down.
func limited(rt Route, next http.Handler) http.Handler {
	key := routeKey(rt)
	max := perMinute(rt)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := helpers.GetApiTokenID(r.Context())
		if token == "" || !redisStore.IsAvailable() {
			next.ServeHTTP(w, r)
			return
		}
		res := redisStore.AllowFixedWindow(r.Context(), registry.ApiRouteRate, []string{token, key}, max)
		if !res.Allowed {
			after := res.RetryAfterSeconds()
			if after <= 0 {
				after = int(registry.ApiRouteRate.TTL.Seconds())
			}
			w.Header().Set("Retry-After", strconv.Itoa(after))
			writeError(w, Errorf(http.StatusTooManyRequests, "rate_limited", "wait for Retry-After seconds, then send it again",
				"this token has made %d calls to %s in the last minute, its limit", max, key))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// kept is a write's answer under its Idempotency-Key.
type kept struct {
	Pending     bool   `json:"pending,omitempty"`
	BodyHash    string `json:"body_hash"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// idempotent answers a write sent again under the same Idempotency-Key from
// its first answer, and refuses the key with another body. A write in
// progress under the key is a 409. A first answer of 5xx isn't kept, so the
// write can be tried again. Without Redis the write simply runs.
func idempotent(rt Route, next http.Handler) http.Handler {
	route := routeKey(rt)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		token := helpers.GetApiTokenID(r.Context())
		if key == "" || token == "" || !redisStore.IsAvailable() {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > maxIdempotencyKey {
			writeError(w, Errorf(http.StatusBadRequest, "idempotency_key_too_long", "use a UUID",
				"an Idempotency-Key is at most %d characters", maxIdempotencyKey))
			return
		}
		body, err := PeekBody(r)
		if err != nil {
			writeError(w, Errorf(http.StatusBadRequest, "unreadable_body", "", "the request's body couldn't be read: %v", err))
			return
		}
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		args := []string{token, route, key}

		pending, _ := json.Marshal(kept{Pending: true, BodyHash: hash})
		claimed, _ := redisStore.SetStringIfAbsent(r.Context(), registry.ApiIdempotency, args, string(pending), idempotencyFor)
		if !claimed {
			raw, found, _ := redisStore.GetString(r.Context(), registry.ApiIdempotency, args)
			var k kept
			if found && json.Unmarshal([]byte(raw), &k) == nil {
				switch {
				case k.BodyHash != hash:
					writeError(w, Errorf(http.StatusUnprocessableEntity, "idempotency_key_reused", "send a new Idempotency-Key with a different request",
						"this Idempotency-Key was sent with another body in the last 24 hours"))
				case k.Pending:
					writeError(w, Errorf(http.StatusConflict, "in_progress", "wait a moment, then send it again with the same key",
						"a request with this Idempotency-Key is still being answered"))
				default:
					if k.ContentType != "" {
						w.Header().Set("Content-Type", k.ContentType)
					}
					w.Header().Set("Idempotent-Replayed", "true")
					w.WriteHeader(k.Status)
					_, _ = w.Write(k.Body)
				}
				return
			}
			// Gone between the claim and the read: run it as a first send.
		}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 500 {
			_ = redisStore.Delete(context.WithoutCancel(r.Context()), registry.ApiIdempotency, args)
			return
		}
		done, _ := json.Marshal(kept{BodyHash: hash, Status: rec.status, ContentType: w.Header().Get("Content-Type"), Body: rec.body.Bytes()})
		_ = redisStore.SetStringWithTTL(context.WithoutCancel(r.Context()), registry.ApiIdempotency, args, string(done), idempotencyFor)
	})
}

// recorder keeps a copy of what a handler wrote.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// serve runs a route in the new shape: its value as JSON, its error as the
// one error body, an ETag for a versioned value.
func serve(rt Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, Errorf(http.StatusBadRequest, "unreadable_body", "", "the request's body couldn't be read: %v", err))
				return
			}
			body = b
		}
		v, err := rt.Serve(r.Context(), Request{HTTP: r, Body: body})
		if err != nil {
			writeError(w, asError(err))
			return
		}
		if rt.Versioned {
			if ver, ok := v.(Versioned); ok {
				w.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, ver.APIVersion()))
			}
		}
		status := http.StatusOK
		if rt.Write && r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		helpers.WriteJSON(w, status, v)
	})
}

// asError is err as the API says it: an *Error as it is, anything else a 500
// that says what to do, never what broke inside.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Errorf(http.StatusInternalServerError, "internal", "try again; if it keeps failing, tell your workspace's admin the X-Request-Id",
		"something went wrong on the server")
}

// errorBody is an Error as it goes on the wire: a plain value, because
// WriteJSON redacts anything that is an error (helpers.RedactErrorsInResponse).
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// writeError writes the one error body.
func writeError(w http.ResponseWriter, e *Error) {
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	helpers.WriteJSON(w, status, map[string]any{"error": errorBody{Code: e.Code, Message: e.Message, Hint: e.Hint}})
}
